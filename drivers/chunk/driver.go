package chunk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
)

const (
	chunkPrefix = "[openlist_chunk]"
	hashPrefix  = "hash_"
)

// Shared helper functions to reduce code duplication and improve reusability
func (d *Chunk) getRemotePath(path string) (driver.Driver, string, error) {
	remoteStorage, remoteActualPath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return nil, "", err
	}
	return remoteStorage, stdpath.Join(remoteActualPath, path), nil
}

func (d *Chunk) getChunkDirName(fileName string) string {
	return chunkPrefix + fileName
}

func (d *Chunk) isChunkDir(name string) (string, bool) {
	return strings.CutPrefix(name, chunkPrefix)
}

func (d *Chunk) isHashFile(name string) (string, string, bool) {
	name = strings.TrimSuffix(name, d.CustomExt)
	if after, ok := strings.CutPrefix(name, hashPrefix); ok {
		hn, value, ok := strings.Cut(after, "_")
		return hn, value, ok
	}
	return "", "", false
}

func (d *Chunk) parseChunkIndex(name string) (int, bool) {
	idx, err := strconv.Atoi(strings.TrimSuffix(name, d.CustomExt))
	return idx, err == nil
}

func (d *Chunk) validateChunkIntegrity(chunkSizes []int64) error {
	if len(chunkSizes) == 0 {
		return fmt.Errorf("no chunks found for file")
	}
	// Only check chunks up to the second-to-last one (last chunk can be partial)
	for i := 0; i < len(chunkSizes)-1; i++ {
		if chunkSizes[i] <= 0 {
			return fmt.Errorf("chunk part[%d] is missing or has invalid size", i)
		}
	}
	return nil
}

// logDebug logs debug messages only when debug mode is enabled
func (d *Chunk) logDebug(format string, args ...interface{}) {
	if flags.Debug || flags.Dev {
		utils.Log.Debugf("[chunk] "+format, args...)
	}
}

// logError logs error messages with proper context
func (d *Chunk) logError(err error, context string) {
	if flags.Debug || flags.Dev {
		utils.Log.Errorf("[chunk] %s: %+v", context, err)
	} else {
		utils.Log.Errorf("[chunk] %s: %v", context, err)
	}
}

type Chunk struct {
	model.Storage
	Addition
}

type chunkMetadata struct {
	TotalSize  int64
	Hash       map[*utils.HashType]string
	ModTime    time.Time
	CreateTime time.Time
	CachedAt   time.Time
}

func (d *Chunk) Config() driver.Config {
	return config
}

func (d *Chunk) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Chunk) Init(ctx context.Context) error {
	if d.PartSize <= 0 {
		return errors.New("part size must be positive")
	}
	d.RemotePath = utils.FixAndCleanPath(d.RemotePath)
	return nil
}

func (d *Chunk) Drop(ctx context.Context) error {
	return nil
}

func (d *Chunk) Get(ctx context.Context, path string) (model.Obj, error) {
	if utils.PathEqual(path, "/") {
		return &model.Object{
			Name:     "Root",
			IsFolder: true,
			Path:     "/",
		}, nil
	}
	
	remoteStorage, remoteActualPath, err := d.getRemotePath(path)
	if err != nil {
		return nil, err
	}
	
	// Try to get as regular file first
	if remoteObj, err := op.Get(ctx, remoteStorage, remoteActualPath); err == nil {
		return &model.Object{
			Path:     path,
			Name:     remoteObj.GetName(),
			Size:     remoteObj.GetSize(),
			Modified: remoteObj.ModTime(),
			IsFolder: remoteObj.IsDir(),
			HashInfo: remoteObj.GetHash(),
		}, nil
	}

	// Try as chunked file
	remoteActualDir, name := stdpath.Split(remoteActualPath)
	chunkDirPath := stdpath.Join(remoteActualDir, d.getChunkDirName(name))
	
	chunkObjs, err := op.List(ctx, remoteStorage, chunkDirPath, model.ListArgs{})
	if err != nil {
		return nil, err
	}
	
	return d.buildChunkObject(chunkObjs, path, name)
}

// buildChunkObject processes chunk objects and builds a chunkObject
func (d *Chunk) buildChunkObject(chunkObjs []model.Obj, path, name string) (*chunkObject, error) {
	var totalSize int64
	h := make(map[*utils.HashType]string)
	var first model.Obj
	
	// Pre-scan to find max chunk index for efficient allocation
	maxIdx := -1
	for _, o := range chunkObjs {
		if o.IsDir() {
			continue
		}
		
		// Skip hash files in first pass
		if _, _, ok := d.isHashFile(o.GetName()); ok {
			continue
		}
		
		if idx, ok := d.parseChunkIndex(o.GetName()); ok && idx > maxIdx {
			maxIdx = idx
		}
	}
	
	if maxIdx < 0 {
		return nil, fmt.Errorf("no valid chunk files found")
	}
	
	// Allocate chunkSizes array with known size
	chunkSizes := make([]int64, maxIdx+1)
	for i := range chunkSizes {
		chunkSizes[i] = -1 // Mark as missing
	}
	
	// Second pass to fill data
	for _, o := range chunkObjs {
		if o.IsDir() {
			continue
		}
		
		// Process hash files
		if hn, value, ok := d.isHashFile(o.GetName()); ok {
			if ht, ok := utils.GetHashByName(hn); ok {
				h[ht] = value
			}
			continue
		}
		
		// Process chunk files
		if idx, ok := d.parseChunkIndex(o.GetName()); ok {
			totalSize += o.GetSize()
			if idx == 0 {
				first = o
			}
			chunkSizes[idx] = o.GetSize()
		}
	}
	
	if err := d.validateChunkIntegrity(chunkSizes); err != nil {
		return nil, err
	}
	
	reqDir, _ := stdpath.Split(path)
	objRes := chunkObject{
		Object: model.Object{
			Path:     stdpath.Join(reqDir, d.getChunkDirName(name)),
			Name:     name,
			Size:     totalSize,
			Modified: first.ModTime(),
			Ctime:    first.CreateTime(),
		},
		chunkSizes: chunkSizes,
	}
	
	if len(h) > 0 {
		objRes.HashInfo = utils.NewHashInfoByMap(h)
	}
	
	return &objRes, nil
}
func (d *Chunk) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	remoteStorage, remoteActualDir, err := d.getRemotePath(dir.GetPath())
	if err != nil {
		return nil, err
	}
	
	remoteObjs, err := op.List(ctx, remoteStorage, remoteActualDir, model.ListArgs{
		ReqPath: args.ReqPath,
		Refresh: args.Refresh,
	})
	if err != nil {
		return nil, err
	}
	
	result := make([]model.Obj, 0, len(remoteObjs))
	for _, obj := range remoteObjs {
		rawName := obj.GetName()
		
		// Process chunk directories
		if obj.IsDir() {
			if name, ok := d.isChunkDir(rawName); ok {
				if chunkObj, err := d.processChunkDir(ctx, remoteStorage, remoteActualDir, rawName, name, args); err == nil {
					result = append(result, chunkObj)
				} else {
					d.logDebug("skipping invalid chunk directory %s", rawName)
				}
				continue
			}
		}
		
		// Skip other chunk-related files
		if strings.HasPrefix(rawName, chunkPrefix) {
			continue
		}

		// Skip hidden files if configured
		if !d.ShowHidden && strings.HasPrefix(rawName, ".") {
			continue
		}
		
		// Add regular files/directories
		result = append(result, d.createRegularObject(obj))
	}
	
	return result, nil
}

// processChunkDir handles chunk directory processing with error recovery
func (d *Chunk) processChunkDir(ctx context.Context, remoteStorage driver.Driver, remoteActualDir, rawName, name string, args model.ListArgs) (model.Obj, error) {
	chunkDirPath := stdpath.Join(remoteActualDir, rawName)
	metadata, err := d.getChunkMetadata(ctx, remoteStorage, chunkDirPath, args)
	if err != nil {
		d.logDebug("failed to get chunk metadata for %s: %v", name, err)
		return nil, err
	}
	
	objRes := model.Object{
		Name:     name,
		Size:     metadata.TotalSize,
		Modified: metadata.ModTime,
		Ctime:    metadata.CreateTime,
	}
	
	if len(metadata.Hash) > 0 {
		objRes.HashInfo = utils.NewHashInfoByMap(metadata.Hash)
	}
	
	if !d.Thumbnail {
		return &objRes, nil
	}
	
	// Create thumbnail object with reused path generation
	thumbPath := stdpath.Join(args.ReqPath, ".thumbnails", name+".webp")
	thumb := fmt.Sprintf("%s/d%s?sign=%s",
		common.GetApiUrl(ctx),
		utils.EncodePath(thumbPath, true),
		sign.Sign(thumbPath))
	
	return &model.ObjThumb{
		Object: objRes,
		Thumbnail: model.Thumbnail{
			Thumbnail: thumb,
		},
	}, nil
}

// createRegularObject efficiently creates standard file/directory objects
func (d *Chunk) createRegularObject(obj model.Obj) model.Obj {
	objRes := model.Object{
		Name:     obj.GetName(),
		Size:     obj.GetSize(),
		Modified: obj.ModTime(),
		IsFolder: obj.IsDir(),
		HashInfo: obj.GetHash(),
	}
	
	if thumb, ok := model.GetThumb(obj); ok {
		return &model.ObjThumb{
			Object: objRes,
			Thumbnail: model.Thumbnail{
				Thumbnail: thumb,
			},
		}
	}
	
	return &objRes
}
func (d *Chunk) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	remoteStorage, remoteActualPath, err := d.getRemotePath(file.GetPath())
	if err != nil {
		return nil, err
	}
	
	chunkFile, ok := file.(*chunkObject)
	if !ok {
		// Regular file, use standard link
		l, _, err := op.Link(ctx, remoteStorage, remoteActualPath, args)
		if err != nil {
			return nil, err
		}
		resultLink := *l
		resultLink.SyncClosers = utils.NewSyncClosers(l)
		return &resultLink, nil
	}
	
	// Chunk file: create merged range reader
	fileSize := chunkFile.GetSize()
	mergedRrf := d.createMergedRangeReader(ctx, chunkFile, remoteStorage, remoteActualPath, args, fileSize)
	
	return &model.Link{
		RangeReader: stream.RangeReaderFunc(mergedRrf),
	}, nil
}

// createMergedRangeReader creates a range reader that merges multiple chunks
func (d *Chunk) createMergedRangeReader(ctx context.Context, chunkFile *chunkObject, remoteStorage driver.Driver, remoteActualPath string, args model.LinkArgs, fileSize int64) func(context.Context, http_range.Range) (io.ReadCloser, error) {
	return func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		start := httpRange.Start
		length := httpRange.Length
		if length < 0 || start+length > fileSize {
			length = fileSize - start
		}
		if length == 0 {
			return io.NopCloser(strings.NewReader("")), nil
		}
		
		rs := make([]io.Reader, 0)
		cs := make(utils.Closers, 0)
		var readFrom bool
		
		for idx, chunkSize := range chunkFile.chunkSizes {
			if readFrom {
				if newLength, reader, err := d.processReadingChunk(ctx, chunkFile, remoteStorage, remoteActualPath, args, idx, chunkSize, length, &cs); err != nil {
					_ = cs.Close()
					return nil, err
				} else {
					rs = append(rs, reader)
					cs = append(cs, reader)
					if newLength <= 0 {
						return utils.ReadCloser{
							Reader: io.MultiReader(rs...),
							Closer: &cs,
						}, nil
					}
					length = newLength
				}
			} else if newStart := start - chunkSize; newStart >= 0 {
				start = newStart
			} else {
				if newLength, reader, err := d.processStartingChunk(ctx, chunkFile, remoteStorage, remoteActualPath, args, idx, chunkSize, start, length, &cs); err != nil {
					_ = cs.Close()
					return nil, err
				} else {
					length = newLength
					cs = append(cs, reader)
					if length <= 0 {
						return utils.ReadCloser{
							Reader: reader,
							Closer: &cs,
						}, nil
					}
					rs = append(rs, reader)
					readFrom = true
				}
			}
		}
		return nil, fmt.Errorf("invalid range: start=%d,length=%d,fileSize=%d", httpRange.Start, httpRange.Length, fileSize)
	}
}

// processReadingChunk processes a chunk when already reading
func (d *Chunk) processReadingChunk(ctx context.Context, chunkFile *chunkObject, remoteStorage driver.Driver, remoteActualPath string, args model.LinkArgs, idx int, chunkSize, length int64, cs *utils.Closers) (int64, io.ReadCloser, error) {
	l, o, err := d.getLinkForChunk(ctx, chunkFile, remoteStorage, stdpath.Join(remoteActualPath, d.getPartName(idx)), args)
	if err != nil {
		d.logDebug("failed to get link for chunk %d: %v", idx, err)
		return 0, nil, err
	}
	*cs = append(*cs, l)
	
	chunkSize2 := l.ContentLength
	if chunkSize2 <= 0 {
		chunkSize2 = o.GetSize()
	}
	if chunkSize2 != chunkSize {
		return 0, nil, fmt.Errorf("chunk part[%d] size not match", idx)
	}
	
	rrf, err := stream.GetRangeReaderFromLink(chunkSize2, l)
	if err != nil {
		d.logDebug("failed to create range reader for chunk %d: %v", idx, err)
		return 0, nil, err
	}
	
	newLength := length - chunkSize2
	var rc io.ReadCloser
	if newLength >= 0 {
		rc, err = rrf.RangeRead(ctx, http_range.Range{Length: -1})
	} else {
		rc, err = rrf.RangeRead(ctx, http_range.Range{Length: length})
	}
	
	if err != nil {
		d.logDebug("failed to read range for chunk %d: %v", idx, err)
	}
	
	return newLength, rc, err
}

// processStartingChunk processes the first chunk in a range
func (d *Chunk) processStartingChunk(ctx context.Context, chunkFile *chunkObject, remoteStorage driver.Driver, remoteActualPath string, args model.LinkArgs, idx int, chunkSize, start, length int64, cs *utils.Closers) (int64, io.ReadCloser, error) {
	l, o, err := d.getLinkForChunk(ctx, chunkFile, remoteStorage, stdpath.Join(remoteActualPath, d.getPartName(idx)), args)
	if err != nil {
		d.logDebug("failed to get link for starting chunk %d: %v", idx, err)
		return 0, nil, err
	}
	*cs = append(*cs, l)
	
	chunkSize2 := l.ContentLength
	if chunkSize2 <= 0 {
		chunkSize2 = o.GetSize()
	}
	if chunkSize2 != chunkSize {
		return 0, nil, fmt.Errorf("chunk part[%d] size not match", idx)
	}
	
	rrf, err := stream.GetRangeReaderFromLink(chunkSize2, l)
	if err != nil {
		d.logDebug("failed to create range reader for starting chunk %d: %v", idx, err)
		return 0, nil, err
	}
	
	rc, err := rrf.RangeRead(ctx, http_range.Range{Start: start, Length: -1})
	if err != nil {
		d.logDebug("failed to read range for starting chunk %d: %v", idx, err)
		return 0, nil, err
	}
	
	newLength := length - (chunkSize2 - start)
	return newLength, rc, nil
}

func (d *Chunk) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	remoteStorage, targetPath, err := d.getRemotePath(stdpath.Join(parentDir.GetPath(), dirName))
	if err != nil {
		return err
	}
	return op.MakeDir(ctx, remoteStorage, targetPath)
}

func (d *Chunk) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	remoteStorage, srcPath, err := d.getRemotePath(srcObj.GetPath())
	if err != nil {
		return err
	}
	_, dstPath, err := d.getRemotePath(dstDir.GetPath())
	if err != nil {
		return err
	}
	return op.Move(ctx, remoteStorage, srcPath, dstPath)
}

func (d *Chunk) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	remoteStorage, srcPath, err := d.getRemotePath(srcObj.GetPath())
	if err != nil {
		return err
	}
	
	if _, ok := srcObj.(*chunkObject); !ok {
		return op.Rename(ctx, remoteStorage, srcPath, newName)
	}
	
	// Chunk files are stored in prefixed directories
	return op.Rename(ctx, remoteStorage, srcPath, d.getChunkDirName(newName))
}

func (d *Chunk) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	remoteStorage, srcPath, err := d.getRemotePath(srcObj.GetPath())
	if err != nil {
		return err
	}
	_, dstPath, err := d.getRemotePath(dstDir.GetPath())
	if err != nil {
		return err
	}
	return op.Copy(ctx, remoteStorage, srcPath, dstPath)
}

func (d *Chunk) Remove(ctx context.Context, obj model.Obj) error {
	remoteStorage, targetPath, err := d.getRemotePath(obj.GetPath())
	if err != nil {
		return err
	}
	return op.Remove(ctx, remoteStorage, targetPath)
}

func (d *Chunk) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	remoteStorage, dstPath, err := d.getRemotePath(dstDir.GetPath())
	if err != nil {
		return err
	}
	
	// Handle thumbnail uploads directly
	if d.Thumbnail && dstDir.GetName() == ".thumbnails" {
		return op.Put(ctx, remoteStorage, dstPath, file, up)
	}
	
	upReader := &driver.ReaderUpdatingProgress{
		Reader:         file,
		UpdateProgress: up,
	}
	
	chunkDirPath := stdpath.Join(dstPath, d.getChunkDirName(file.GetName()))
	
	// Store hash files if enabled
	if d.StoreHash {
		if err := d.storeHashFiles(ctx, remoteStorage, chunkDirPath, file); err != nil {
			d.logDebug("failed to store hash files: %v", err)
			// Continue without hash files as it's not critical
		}
	}
	
	// Upload chunk parts
	return d.uploadChunkParts(ctx, remoteStorage, chunkDirPath, file, upReader)
}

// storeHashFiles stores hash files for the uploaded file
func (d *Chunk) storeHashFiles(ctx context.Context, remoteStorage driver.Driver, chunkDirPath string, file model.FileStreamer) error {
	for ht, value := range file.GetHash().All() {
		hashFileName := fmt.Sprintf("%s%s_%s%s", hashPrefix, ht.Name, value, d.CustomExt)
		err := op.Put(ctx, remoteStorage, chunkDirPath, &stream.FileStream{
			Obj: &model.Object{
				Name:     hashFileName,
				Size:     1,
				Modified: file.ModTime(),
			},
			Mimetype: "application/octet-stream",
			Reader:   bytes.NewReader([]byte{0}), // Compatible with drivers that don't support empty files
		}, nil, true)
		if err != nil {
			d.logDebug("failed to store hash file %s: %v", hashFileName, err)
			// Continue with other hash files
		}
	}
	return nil
}

// uploadChunkParts uploads all file chunks with proper error handling
func (d *Chunk) uploadChunkParts(ctx context.Context, remoteStorage driver.Driver, chunkDirPath string, file model.FileStreamer, upReader io.Reader) error {
	fileSize := file.GetSize()
	fullPartCount := int(fileSize / d.PartSize)
	tailSize := fileSize % d.PartSize
	
	// Adjust for files that end exactly on chunk boundary
	if tailSize == 0 && fullPartCount > 0 {
		fullPartCount--
		tailSize = d.PartSize
	}
	
	// Use helper function to reduce code duplication
	cleanupOnError := func(err error) error {
		_ = op.Remove(ctx, remoteStorage, chunkDirPath)
		return err
	}
	
	// Upload full-size chunks
	for partIndex := 0; partIndex < fullPartCount; partIndex++ {
		if err := d.uploadSingleChunk(ctx, remoteStorage, chunkDirPath, file, upReader, partIndex, d.PartSize); err != nil {
			d.logError(err, fmt.Sprintf("upload chunk %d", partIndex))
			return cleanupOnError(fmt.Errorf("failed to upload chunk %d: %w", partIndex, err))
		}
	}
	
	// Upload final chunk (may be smaller)
	if err := d.uploadSingleChunk(ctx, remoteStorage, chunkDirPath, file, upReader, fullPartCount, tailSize); err != nil {
		d.logError(err, "upload final chunk")
		return cleanupOnError(fmt.Errorf("failed to upload final chunk: %w", err))
	}
	
	return nil
}

// uploadSingleChunk uploads a single chunk
func (d *Chunk) uploadSingleChunk(ctx context.Context, remoteStorage driver.Driver, chunkDirPath string, file model.FileStreamer, upReader io.Reader, partIndex int, chunkSize int64) error {
	partName := d.getPartName(partIndex)
	return op.Put(ctx, remoteStorage, chunkDirPath, &stream.FileStream{
		Obj: &model.Object{
			Name:     partName,
			Size:     chunkSize,
			Modified: file.ModTime(),
		},
		Mimetype: file.GetMimetype(),
		Reader:   io.LimitReader(upReader, chunkSize),
	}, nil, true)
}

func (d *Chunk) getPartName(part int) string {
	return fmt.Sprintf("%d%s", part, d.CustomExt)
}

// getLinkForChunk directly uses op.Link for caching
func (d *Chunk) getLinkForChunk(ctx context.Context, chunkFile *chunkObject, remoteStorage driver.Driver, chunkPath string, args model.LinkArgs) (*model.Link, model.Obj, error) {
	return op.Link(ctx, remoteStorage, chunkPath, args)
}

// getChunkMetadata retrieves chunk directory metadata efficiently
func (d *Chunk) getChunkMetadata(ctx context.Context, remoteStorage driver.Driver, chunkDirPath string, args model.ListArgs) (*chunkMetadata, error) {
	chunkObjs, err := op.List(ctx, remoteStorage, chunkDirPath, model.ListArgs{
		ReqPath: args.ReqPath,
		Refresh: args.Refresh,
	})
	if err != nil {
		return nil, err
	}

	metadata := &chunkMetadata{
		Hash:     make(map[*utils.HashType]string),
		CachedAt: time.Now(),
	}
	
	for _, o := range chunkObjs {
		if o.IsDir() {
			continue
		}

		// Process hash files
		if hn, value, ok := d.isHashFile(o.GetName()); ok {
			if ht, ok := utils.GetHashByName(hn); ok {
				metadata.Hash[ht] = value
			}
			continue
		}

		// Process chunk files
		if idx, ok := d.parseChunkIndex(o.GetName()); ok {
			if idx == 0 {
				metadata.ModTime = o.ModTime()
				metadata.CreateTime = o.CreateTime()
			}
			metadata.TotalSize += o.GetSize()
		}
	}

	return metadata, nil
}

var _ driver.Driver = (*Chunk)(nil)
