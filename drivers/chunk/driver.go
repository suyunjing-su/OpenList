package chunk

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

type Chunk struct {
	model.Storage
	Addition
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
	storage, reqActualPath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return nil, err
	}
	reqPath := stdpath.Join(reqActualPath, path)
	obj, err := op.Get(ctx, storage, reqPath)
	if err == nil {
		return &model.Object{
			Path:     path,
			Name:     obj.GetName(),
			Size:     obj.GetSize(),
			Modified: obj.ModTime(),
			IsFolder: obj.IsDir(),
			HashInfo: obj.GetHash(),
		}, nil
	}

	dir, base := stdpath.Split(reqPath)
	reqPath = stdpath.Join(dir, "[openlist_chunk]"+base)
	obj, err = op.Get(ctx, storage, reqPath)
	if err != nil {
		return nil, err
	}
	chunkObjs, err := op.List(ctx, storage, reqPath, model.ListArgs{})
	if err != nil {
		return nil, err
	}
	var totalSize int64 = 0
	chunkSizes := []int64{-1}
	for _, o := range chunkObjs {
		if o.IsDir() {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSuffix(o.GetName(), d.CustomExt))
		if err != nil {
			continue
		}
		totalSize += o.GetSize()
		if len(chunkSizes) > idx {
			chunkSizes[idx] = o.GetSize()
		} else if len(chunkSizes) == idx {
			chunkSizes = append(chunkSizes, o.GetSize())
		} else {
			newChunkSizes := make([]int64, idx+1)
			copy(newChunkSizes, chunkSizes)
			chunkSizes = newChunkSizes
			chunkSizes[idx] = o.GetSize()
		}
	}
	for i, l := 0, len(chunkSizes)-2; i <= l; i++ {
		if (i == 0 && chunkSizes[i] == -1) || chunkSizes[i] == 0 {
			return nil, fmt.Errorf("chunk part[%d] are missing", i)
		}
	}
	reqDir, _ := stdpath.Split(path)
	return &chunkObject{
		Object: model.Object{
			Path:     stdpath.Join(reqDir, "[openlist_chunk]"+base),
			Name:     base,
			Size:     totalSize,
			Modified: obj.ModTime(),
			Ctime:    obj.CreateTime(),
		},
		chunkSizes: chunkSizes,
	}, nil
}

func (d *Chunk) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	storage, reqActualPath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return nil, err
	}
	dirPath := stdpath.Join(reqActualPath, dir.GetPath())
	objs, err := op.List(ctx, storage, dirPath, model.ListArgs{Refresh: args.Refresh})
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(objs, func(obj model.Obj) (model.Obj, error) {
		if !obj.IsDir() || !strings.HasPrefix(obj.GetName(), "[openlist_chunk]") {
			thumb, ok := model.GetThumb(obj)
			objRes := model.Object{
				Name:     obj.GetName(),
				Size:     obj.GetSize(),
				Modified: obj.ModTime(),
				IsFolder: obj.IsDir(),
			}
			if !ok {
				return &objRes, nil
			}
			return &model.ObjThumb{
				Object: objRes,
				Thumbnail: model.Thumbnail{
					Thumbnail: thumb,
				},
			}, nil
		}
		reqPath := stdpath.Join(dirPath, obj.GetName())
		chunkObjs, err := op.List(ctx, storage, reqPath, model.ListArgs{Refresh: args.Refresh})
		if err != nil {
			return nil, err
		}
		totalSize := int64(0)
		for _, o := range chunkObjs {
			if o.IsDir() {
				continue
			}
			_, err := strconv.Atoi(strings.TrimSuffix(o.GetName(), d.CustomExt))
			if err != nil {
				continue
			}
			totalSize += o.GetSize()
		}
		return &model.Object{
			Name:     strings.TrimPrefix(obj.GetName(), "[openlist_chunk]"),
			Path:     reqPath,
			Size:     totalSize,
			Modified: obj.ModTime(),
			Ctime:    obj.CreateTime(),
		}, nil
	})
}

func (d *Chunk) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	storage, reqActualPath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return nil, err
	}
	args.Redirect = false
	chunkFile, ok := file.(*chunkObject)
	reqPath := stdpath.Join(reqActualPath, file.GetPath())
	if !ok {
		l, _, err := op.Link(ctx, storage, reqPath, args)
		if err != nil {
			return nil, err
		}
		resultLink := *l
		resultLink.SyncClosers = utils.NewSyncClosers(l)
		return &resultLink, nil
	}
	fileSize := chunkFile.GetSize()
	mergedRrf := func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
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
		var (
			rc       io.ReadCloser
			readFrom bool
		)
		for idx, chunkSize := range chunkFile.chunkSizes {
			if readFrom {
				l, o, err := op.Link(ctx, storage, stdpath.Join(reqPath, d.getPartName(idx)), args)
				if err != nil {
					_ = cs.Close()
					return nil, err
				}
				cs = append(cs, l)
				chunkSize2 := l.ContentLength
				if chunkSize2 <= 0 {
					chunkSize2 = o.GetSize()
				}
				if chunkSize2 != chunkSize {
					_ = cs.Close()
					return nil, fmt.Errorf("chunk part[%d] size not match", idx)
				}
				rrf, err := stream.GetRangeReaderFromLink(chunkSize2, l)
				if err != nil {
					_ = cs.Close()
					return nil, err
				}
				newLength := length - chunkSize2
				if newLength >= 0 {
					length = newLength
					rc, err = rrf.RangeRead(ctx, http_range.Range{Length: -1})
				} else {
					rc, err = rrf.RangeRead(ctx, http_range.Range{Length: length})
				}
				if err != nil {
					_ = cs.Close()
					return nil, err
				}
				rs = append(rs, rc)
				cs = append(cs, rc)
				if newLength <= 0 {
					return utils.ReadCloser{
						Reader: io.MultiReader(rs...),
						Closer: &cs,
					}, nil
				}
			} else if newStart := start - chunkSize; newStart >= 0 {
				start = newStart
			} else {
				l, o, err := op.Link(ctx, storage, stdpath.Join(reqPath, d.getPartName(idx)), args)
				if err != nil {
					_ = cs.Close()
					return nil, err
				}
				cs = append(cs, l)
				chunkSize2 := l.ContentLength
				if chunkSize2 <= 0 {
					chunkSize2 = o.GetSize()
				}
				if chunkSize2 != chunkSize {
					_ = cs.Close()
					return nil, fmt.Errorf("chunk part[%d] size not match", idx)
				}
				rrf, err := stream.GetRangeReaderFromLink(chunkSize2, l)
				if err != nil {
					_ = cs.Close()
					return nil, err
				}
				rc, err = rrf.RangeRead(ctx, http_range.Range{Start: start, Length: -1})
				if err != nil {
					return nil, err
				}
				length -= chunkSize2 - start
				cs = append(cs, rc)
				if length <= 0 {
					return utils.ReadCloser{
						Reader: rc,
						Closer: &cs,
					}, nil
				}
				rs = append(rs, rc)
				readFrom = true
			}
		}
		return nil, fmt.Errorf("invalid range: start=%d,length=%d,fileSize=%d", httpRange.Start, httpRange.Length, fileSize)
	}
	return &model.Link{
		RangeReader: stream.RangeReaderFunc(mergedRrf),
	}, nil
}

func (d *Chunk) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	path := stdpath.Join(d.RemotePath, parentDir.GetPath(), dirName)
	return fs.MakeDir(ctx, path)
}

func (d *Chunk) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	src := stdpath.Join(d.RemotePath, srcObj.GetPath())
	dst := stdpath.Join(d.RemotePath, dstDir.GetPath())
	_, err := fs.Move(ctx, src, dst)
	return err
}

func (d *Chunk) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	chunkObj, ok := srcObj.(*chunkObject)
	if !ok {
		return fs.Rename(ctx, stdpath.Join(d.RemotePath, srcObj.GetPath()), newName)
	}
	return fs.Rename(ctx, stdpath.Join(d.RemotePath, chunkObj.GetPath()), "[openlist_chunk]"+newName)
}

func (d *Chunk) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	dst := stdpath.Join(d.RemotePath, dstDir.GetPath())
	src := stdpath.Join(d.RemotePath, srcObj.GetPath())
	_, err := fs.Copy(ctx, src, dst)
	return err
}

func (d *Chunk) Remove(ctx context.Context, obj model.Obj) error {
	return fs.Remove(ctx, stdpath.Join(d.RemotePath, obj.GetPath()))
}

func (d *Chunk) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	storage, reqActualPath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return err
	}
	upReader := &driver.ReaderUpdatingProgress{
		Reader:         file,
		UpdateProgress: up,
	}
	dst := stdpath.Join(reqActualPath, dstDir.GetPath(), "[openlist_chunk]"+file.GetName())
	fullPartCount := int(file.GetSize() / d.PartSize)
	tailSize := file.GetSize() % d.PartSize
	if tailSize == 0 && fullPartCount > 0 {
		fullPartCount--
		tailSize = d.PartSize
	}
	partIndex := 0
	for partIndex < fullPartCount {
		err = errors.Join(err, op.Put(ctx, storage, dst, &stream.FileStream{
			Obj: &model.Object{
				Name:     d.getPartName(partIndex),
				Size:     d.PartSize,
				Modified: file.ModTime(),
			},
			Mimetype: file.GetMimetype(),
			Reader:   io.LimitReader(upReader, d.PartSize),
		}, nil, true))
		partIndex++
	}
	return errors.Join(err, op.Put(ctx, storage, dst, &stream.FileStream{
		Obj: &model.Object{
			Name:     d.getPartName(fullPartCount),
			Size:     tailSize,
			Modified: file.ModTime(),
		},
		Mimetype: file.GetMimetype(),
		Reader:   upReader,
	}, nil))
}

func (d *Chunk) getPartName(part int) string {
	return fmt.Sprintf("%d%s", part, d.CustomExt)
}

var _ driver.Driver = (*Chunk)(nil)
