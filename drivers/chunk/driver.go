package chunk

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"regexp"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
)

type Chunk struct {
	model.Storage
	Addition
	remoteStorage   driver.Driver
	partNameMatchRe *regexp.Regexp
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
	d.partNameMatchRe = regexp.MustCompile("^(.+)\\.openlist_chunk_(\\d+)" + regexp.QuoteMeta(d.CustomExt) + "$")
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
	dir, base := stdpath.Split(stdpath.Join(d.RemotePath, path))
	objs, err := fs.List(ctx, dir, &fs.ListArgs{NoLog: true})
	if err != nil {
		return nil, err
	}
	partPrefix := base + ".openlist_chunk_"
	var first model.Obj
	var totalSize int64 = 0
	for _, obj := range objs {
		if obj.GetName() == base {
			first = obj
			if obj.IsDir() {
				totalSize = obj.GetSize()
				break
			} else {
				totalSize += obj.GetSize()
			}
		} else if strings.HasPrefix(obj.GetName(), partPrefix) {
			totalSize += obj.GetSize()
		}
	}
	if first == nil {
		return nil, errs.ObjectNotFound
	}
	return &model.Object{
		Path:     path,
		Name:     base,
		Size:     totalSize,
		Modified: first.ModTime(),
		Ctime:    first.CreateTime(),
		IsFolder: first.IsDir(),
	}, nil
}

func (d *Chunk) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	objs, err := fs.List(ctx, stdpath.Join(d.RemotePath, dir.GetPath()), &fs.ListArgs{NoLog: true, Refresh: args.Refresh})
	if err != nil {
		return nil, err
	}
	ret := make([]model.Obj, 0)
	sizeMap := make(map[string]int64)
	for _, obj := range objs {
		if obj.IsDir() {
			ret = append(ret, &model.Object{
				Name:     obj.GetName(),
				Size:     obj.GetSize(),
				Modified: obj.ModTime(),
				Ctime:    obj.CreateTime(),
				IsFolder: true,
			})
			continue
		}
		var name string
		matches := d.partNameMatchRe.FindStringSubmatch(obj.GetName())
		if len(matches) < 3 {
			ret = append(ret, &model.Object{
				Name:     obj.GetName(),
				Size:     0,
				Modified: obj.ModTime(),
				Ctime:    obj.CreateTime(),
				IsFolder: false,
			})
			name = obj.GetName()
		} else {
			name = matches[1]
		}
		_, ok := sizeMap[name]
		if !ok {
			sizeMap[name] = obj.GetSize()
		} else {
			sizeMap[name] += obj.GetSize()
		}
	}
	for _, obj := range ret {
		if !obj.IsDir() {
			obj.(*model.Object).Size = sizeMap[obj.GetName()]
		}
	}
	return ret, nil
}

func (d *Chunk) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	path := stdpath.Join(d.RemotePath, file.GetPath())
	links := make([]*model.Link, 0)
	rrfs := make([]model.RangeReaderIF, 0)
	totalLength := int64(0)
	for {
		l, o, err := link(ctx, d.getPartName(path, len(links)), args)
		if errors.Is(err, errs.ObjectNotFound) {
			break
		}
		if err != nil {
			for _, l1 := range links {
				_ = l1.Close()
			}
			return nil, fmt.Errorf("failed get Part %d link: %+v", len(links), err)
		}
		if l.ContentLength <= 0 {
			l.ContentLength = o.GetSize()
		}
		rrf, err := stream.GetRangeReaderFromLink(l.ContentLength, l)
		if err != nil {
			for _, l1 := range links {
				_ = l1.Close()
			}
			_ = l.Close()
			return nil, fmt.Errorf("failed get Part %d range reader: %+v", len(links), err)
		}
		links = append(links, l)
		rrfs = append(rrfs, rrf)
		totalLength += l.ContentLength
	}
	mergedRrf := func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		if httpRange.Length == -1 {
			httpRange.Length = totalLength - httpRange.Start
		}
		firstPartIdx := 0
		firstPartStart := httpRange.Start
		for firstPartIdx < len(links) && firstPartStart > links[firstPartIdx].ContentLength {
			firstPartStart -= links[firstPartIdx].ContentLength
			firstPartIdx++
		}
		if firstPartIdx == len(links) {
			return nil, io.EOF
		}
		if firstPartStart+httpRange.Length <= links[firstPartIdx].ContentLength {
			return rrfs[firstPartIdx].RangeRead(ctx, http_range.Range{
				Start:  firstPartStart,
				Length: httpRange.Length,
			})
		}

		lastPartIdx := firstPartIdx
		tailLength := firstPartStart + httpRange.Length
		for lastPartIdx < len(links) && tailLength > links[lastPartIdx].ContentLength {
			tailLength -= links[lastPartIdx].ContentLength
			lastPartIdx++
		}
		if lastPartIdx == len(links) || tailLength == 0 {
			lastPartIdx--
			tailLength = links[lastPartIdx].ContentLength
		}

		rs := make([]io.Reader, 0, lastPartIdx-firstPartIdx+1)
		cs := make(utils.Closers, 0, lastPartIdx-firstPartIdx+1)
		firstRc, err := rrfs[firstPartIdx].RangeRead(ctx, http_range.Range{
			Start:  firstPartStart,
			Length: links[firstPartIdx].ContentLength - firstPartStart,
		})
		if err != nil {
			return nil, err
		}
		rs = append(rs, firstRc)
		cs = append(cs, firstRc)
		partIdx := firstPartIdx + 1
		for partIdx < lastPartIdx {
			rc, err := rrfs[partIdx].RangeRead(ctx, http_range.Range{Length: -1})
			if err != nil {
				return nil, err
			}
			rs = append(rs, rc)
			cs = append(cs, rc)
			partIdx++
		}
		lastRc, err := rrfs[lastPartIdx].RangeRead(ctx, http_range.Range{
			Start:  0,
			Length: tailLength,
		})
		if err != nil {
			return nil, err
		}
		rs = append(rs, lastRc)
		cs = append(cs, lastRc)
		return &struct {
			io.Reader
			utils.Closers
		}{
			Reader:  io.MultiReader(rs...),
			Closers: cs,
		}, nil
	}
	linkClosers := make([]io.Closer, 0, len(links))
	for _, l := range links {
		linkClosers = append(linkClosers, l)
	}
	return &model.Link{
		RangeReader: stream.RangeReaderFunc(mergedRrf),
		SyncClosers: utils.NewSyncClosers(linkClosers...),
	}, nil
}

func link(ctx context.Context, reqPath string, args model.LinkArgs) (*model.Link, model.Obj, error) {
	storage, reqActualPath, err := op.GetStorageAndActualPath(reqPath)
	if err != nil {
		return nil, nil, err
	}
	if !args.Redirect {
		return op.Link(ctx, storage, reqActualPath, args)
	}
	obj, err := fs.Get(ctx, reqPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, nil, err
	}
	if common.ShouldProxy(storage, stdpath.Base(reqPath)) {
		return nil, obj, nil
	}
	return op.Link(ctx, storage, reqActualPath, args)
}

func (d *Chunk) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	path := stdpath.Join(d.RemotePath, parentDir.GetPath(), dirName)
	return fs.MakeDir(ctx, path)
}

func (d *Chunk) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	path := stdpath.Join(d.RemotePath, srcObj.GetPath())
	dst := stdpath.Join(d.RemotePath, dstDir.GetPath())
	if srcObj.IsDir() {
		_, err := fs.Move(ctx, path, dst)
		return err
	}
	dir, base := stdpath.Split(path)
	objs, err := fs.List(ctx, dir, &fs.ListArgs{NoLog: true})
	if err != nil {
		return err
	}
	for _, obj := range objs {
		suffix := strings.TrimPrefix(obj.GetName(), base)
		if suffix != obj.GetName() && strings.HasPrefix(suffix, ".openlist_chunk_") {
			_, e := fs.Move(ctx, path+suffix, dst, true)
			err = errors.Join(err, e)
		}
	}
	_, e := fs.Move(ctx, path, dst)
	return errors.Join(err, e)
}

func (d *Chunk) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	path := stdpath.Join(d.RemotePath, srcObj.GetPath())
	if srcObj.IsDir() {
		return fs.Rename(ctx, path, newName)
	}
	dir, base := stdpath.Split(path)
	objs, err := fs.List(ctx, dir, &fs.ListArgs{NoLog: true})
	if err != nil {
		return err
	}
	for _, obj := range objs {
		suffix := strings.TrimPrefix(obj.GetName(), base)
		if suffix != obj.GetName() && strings.HasPrefix(suffix, ".openlist_chunk_") {
			err = errors.Join(err, fs.Rename(ctx, path+suffix, newName+suffix, true))
		}
	}
	return errors.Join(err, fs.Rename(ctx, path, newName))
}

func (d *Chunk) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	path := stdpath.Join(d.RemotePath, srcObj.GetPath())
	dst := stdpath.Join(d.RemotePath, dstDir.GetPath())
	if srcObj.IsDir() {
		_, err := fs.Copy(ctx, path, dst)
		return err
	}
	dir, base := stdpath.Split(path)
	objs, err := fs.List(ctx, dir, &fs.ListArgs{NoLog: true})
	if err != nil {
		return err
	}
	for _, obj := range objs {
		suffix := strings.TrimPrefix(obj.GetName(), base)
		if suffix != obj.GetName() && strings.HasPrefix(suffix, ".openlist_chunk_") {
			_, e := fs.Copy(ctx, path+suffix, dst, true)
			err = errors.Join(err, e)
		}
	}
	_, e := fs.Copy(ctx, path, dst)
	return errors.Join(err, e)
}

func (d *Chunk) Remove(ctx context.Context, obj model.Obj) error {
	path := stdpath.Join(d.RemotePath, obj.GetPath())
	if obj.IsDir() {
		return fs.Remove(ctx, path)
	}
	dir, base := stdpath.Split(path)
	objs, err := fs.List(ctx, dir, &fs.ListArgs{NoLog: true})
	if err != nil {
		return err
	}
	for _, o := range objs {
		suffix := strings.TrimPrefix(o.GetName(), base)
		if suffix != obj.GetName() && strings.HasPrefix(suffix, ".openlist_chunk_") {
			err = errors.Join(err, fs.Remove(ctx, path+suffix))
		}
	}
	return errors.Join(err, fs.Remove(ctx, path))
}

func (d *Chunk) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	upReader := &driver.ReaderUpdatingProgress{
		Reader:         file,
		UpdateProgress: up,
	}
	dst := stdpath.Join(d.RemotePath, dstDir.GetPath())
	fullPartCount := int(file.GetSize() / d.PartSize)
	tailSize := file.GetSize() % d.PartSize
	partIndex := 0
	var err error
	for partIndex < fullPartCount {
		err = errors.Join(err, fs.PutDirectly(ctx, dst, &stream.FileStream{
			Obj: &model.Object{
				Name:     d.getPartName(file.GetName(), partIndex),
				Size:     d.PartSize,
				Modified: file.ModTime(),
			},
			Mimetype: file.GetMimetype(),
			Reader:   io.LimitReader(upReader, d.PartSize),
		}, true))
		partIndex++
	}
	return errors.Join(err, fs.PutDirectly(ctx, dst, &stream.FileStream{
		Obj: &model.Object{
			Name:     d.getPartName(file.GetName(), fullPartCount),
			Size:     tailSize,
			Modified: file.ModTime(),
		},
		Mimetype: file.GetMimetype(),
		Reader:   upReader,
	}))
}

func (d *Chunk) getPartName(name string, part int) string {
	if part == 0 {
		return name
	}
	return fmt.Sprintf("%s.openlist_chunk_%d%s", name, part, d.CustomExt)
}

var _ driver.Driver = (*Chunk)(nil)
