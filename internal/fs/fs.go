package fs

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/model/reqres"
	"github.com/OpenListTeam/OpenList/v4/internal/model/tables"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/task"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
	"github.com/google/uuid"
)

// the param named path of functions in this package is a mount path
// So, the purpose of this package is to convert mount path to actual path
// then pass the actual path to the op package

type ListArgs struct {
	Refresh bool
	NoLog   bool
}

func List(ctx context.Context, path string, args *ListArgs) ([]model.Obj, error) {
	res, err := list(ctx, path, args)
	if err != nil {
		if !args.NoLog {
			log.Errorf("failed list %s: %+v", path, err)
		}
		return nil, err
	}
	return res, nil
}

type GetArgs struct {
	NoLog bool
}

func Get(ctx context.Context, path string, args *GetArgs) (model.Obj, error) {
	res, err := get(ctx, path)
	if err != nil {
		if !args.NoLog {
			log.Warnf("failed get %s: %s", path, err)
		}
		return nil, err
	}
	return res, nil
}

func Link(ctx context.Context, path string, args model.LinkArgs) (*model.Link, model.Obj, error) {
	res, file, err := link(ctx, path, args)
	if err != nil {
		log.Errorf("failed link %s: %+v", path, err)
		return nil, nil, err
	}
	return res, file, nil
}

func MakeDir(ctx context.Context, path string, lazyCache ...bool) error {
	err := makeDir(ctx, path, lazyCache...)
	if err != nil {
		log.Errorf("failed make dir %s: %+v", path, err)
	}
	return err
}

func Move(ctx context.Context, srcPath, dstDirPath string, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	req, err := transfer(ctx, move, srcPath, dstDirPath, lazyCache...)
	if err != nil {
		log.Errorf("failed move %s to %s: %+v", srcPath, dstDirPath, err)
	}
	return req, err
}

func Copy(ctx context.Context, srcObjPath, dstDirPath string, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	res, err := transfer(ctx, copy, srcObjPath, dstDirPath, lazyCache...)
	if err != nil {
		log.Errorf("failed copy %s to %s: %+v", srcObjPath, dstDirPath, err)
	}
	return res, err
}

func Rename(ctx context.Context, srcPath, dstName string, lazyCache ...bool) error {
	err := rename(ctx, srcPath, dstName, lazyCache...)
	if err != nil {
		log.Errorf("failed rename %s to %s: %+v", srcPath, dstName, err)
	}
	return err
}

func Remove(ctx context.Context, path string) error {
	err := remove(ctx, path)
	if err != nil {
		log.Errorf("failed remove %s: %+v", path, err)
	}
	return err
}

func PutDirectly(ctx context.Context, dstDirPath string, file model.FileStreamer, lazyCache ...bool) error {
	err := putDirectly(ctx, dstDirPath, file, lazyCache...)
	if err != nil {
		log.Errorf("failed put %s: %+v", dstDirPath, err)
	}
	return err
}

func PutAsTask(ctx context.Context, dstDirPath string, file model.FileStreamer) (task.TaskExtensionInfo, error) {
	t, err := putAsTask(ctx, dstDirPath, file)
	if err != nil {
		log.Errorf("failed put %s: %+v", dstDirPath, err)
	}
	return t, err
}

func ArchiveMeta(ctx context.Context, path string, args model.ArchiveMetaArgs) (*model.ArchiveMetaProvider, error) {
	meta, err := archiveMeta(ctx, path, args)
	if err != nil {
		log.Errorf("failed get archive meta %s: %+v", path, err)
	}
	return meta, err
}

func ArchiveList(ctx context.Context, path string, args model.ArchiveListArgs) ([]model.Obj, error) {
	objs, err := archiveList(ctx, path, args)
	if err != nil {
		log.Errorf("failed list archive [%s]%s: %+v", path, args.InnerPath, err)
	}
	return objs, err
}

func ArchiveDecompress(ctx context.Context, srcObjPath, dstDirPath string, args model.ArchiveDecompressArgs, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	t, err := archiveDecompress(ctx, srcObjPath, dstDirPath, args, lazyCache...)
	if err != nil {
		log.Errorf("failed decompress [%s]%s: %+v", srcObjPath, args.InnerPath, err)
	}
	return t, err
}

func ArchiveDriverExtract(ctx context.Context, path string, args model.ArchiveInnerArgs) (*model.Link, model.Obj, error) {
	l, obj, err := archiveDriverExtract(ctx, path, args)
	if err != nil {
		log.Errorf("failed extract [%s]%s: %+v", path, args.InnerPath, err)
	}
	return l, obj, err
}

func ArchiveInternalExtract(ctx context.Context, path string, args model.ArchiveInnerArgs) (io.ReadCloser, int64, error) {
	l, obj, err := archiveInternalExtract(ctx, path, args)
	if err != nil {
		log.Errorf("failed extract [%s]%s: %+v", path, args.InnerPath, err)
	}
	return l, obj, err
}

type GetStoragesArgs struct {
}

func GetStorage(path string, args *GetStoragesArgs) (driver.Driver, error) {
	storageDriver, _, err := op.GetStorageAndActualPath(path)
	if err != nil {
		return nil, err
	}
	return storageDriver, nil
}

func Other(ctx context.Context, args model.FsOtherArgs) (interface{}, error) {
	res, err := other(ctx, args)
	if err != nil {
		log.Errorf("failed get other %s: %+v", args.Path, err)
	}
	return res, err
}

func PutURL(ctx context.Context, path, dstName, urlStr string) error {
	storage, dstDirActualPath, err := op.GetStorageAndActualPath(path)
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	if storage.Config().NoUpload {
		return errors.WithStack(errs.UploadNotSupported)
	}
	_, ok := storage.(driver.PutURL)
	_, okResult := storage.(driver.PutURLResult)
	if !ok && !okResult {
		return errs.NotImplement
	}
	return op.PutURL(ctx, storage, dstDirActualPath, dstName, urlStr)
}

/// 分片上传功能--------------------------------------------------------------------

// Preup 预上传
func Preup(c context.Context, s driver.Driver, actualPath string, req *reqres.PreupReq) (*reqres.PreupResp, error) {
	// 检查是否存在未完成的上传任务（用于断点续传）
	wh := map[string]any{}
	wh["dst_path"] = req.Path
	wh["name"] = req.Name
	wh["size"] = req.Size
	wh["status"] = tables.SliceUploadStatusUploading // 只查找正在进行中的任务
	if req.Hash.Md5 != "" {
		wh["hash_md5"] = req.Hash.Md5
	}
	if req.Hash.Sha1 != "" {
		wh["hash_sha1"] = req.Hash.Sha1
	}
	if req.Hash.Md5256KB != "" {
		wh["hash_md5_256kb"] = req.Hash.Md5256KB
	}

	su, err := db.GetSliceUpload(wh)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Error("GetSliceUpload", err)
		return nil, errors.WithStack(err)
	}

	if su.ID != 0 { // 找到未完成的上传任务，支持断点续传
		return &reqres.PreupResp{
			TaskID:            su.TaskID,
			SliceSize:         su.SliceSize,
			SliceCnt:          su.SliceCnt,
			SliceUploadStatus: su.SliceUploadStatus,
		}, nil
	}
	
	srcobj, err := op.Get(c, s, actualPath)
	if err != nil {
		log.Error(err)
		return nil, errors.WithStack(err)
	}
	user, _ := c.Value(conf.UserKey).(*model.User)

	// 生成唯一的TaskID
	taskID := uuid.New().String()
	
	//创建新的上传任务
	createsu := &tables.SliceUpload{
		TaskID:       taskID,
		DstPath:      req.Path,
		DstID:        srcobj.GetID(),
		Size:         req.Size,
		Name:         req.Name,
		HashMd5:      req.Hash.Md5,
		HashMd5256KB: req.Hash.Md5256KB,
		HashSha1:     req.Hash.Sha1,
		Overwrite:    req.Overwrite,
		ActualPath:   actualPath,
		UserID:       user.ID,
		AsTask:       req.AsTask,
	}
	log.Infof("storage mount path %s", s.GetStorage().MountPath)
	switch st := s.(type) {
	case driver.IPreup:
		log.Info("preup support")
		res, err := st.Preup(c, srcobj, req)
		if err != nil {
			log.Error("Preup error", req, err)
			return nil, errors.WithStack(err)
		}
		log.Info("Preup success", res)
		if res.Reuse { //秒传
			return &reqres.PreupResp{
				Reuse:     true,
				SliceCnt:  0,
				SliceSize: res.SliceSize,
				TaskID:    taskID,
			}, nil

		}
		createsu.PreupID = res.PreupID
		createsu.SliceSize = res.SliceSize
		createsu.Server = res.Server
	default:
		log.Info("Preup not support")
		createsu.SliceSize = 10 * utils.MB
	}
	createsu.SliceCnt = uint((req.Size + createsu.SliceSize - 1) / createsu.SliceSize)
	createsu.SliceUploadStatus = make([]byte, (createsu.SliceCnt+7)/8)
	createsu.Status = tables.SliceUploadStatusWaiting // 设置初始状态

	err = db.CreateSliceUpload(createsu)
	if err != nil {
		log.Error("CreateSliceUpload error", createsu, err)
		return nil, errors.WithStack(err)
	}
	return &reqres.PreupResp{
		Reuse:             false,
		SliceUploadStatus: createsu.SliceUploadStatus,
		SliceSize:         createsu.SliceSize,
		SliceCnt:          createsu.SliceCnt,
		TaskID:            createsu.TaskID,
	}, nil

}

type sliceup struct {
	*tables.SliceUpload
	tmpFile *os.File
	sync.Mutex
}

// ensureTmpFile 确保临时文件存在且正确初始化，线程安全
func (su *sliceup) ensureTmpFile() error {
	su.Lock()
	defer su.Unlock()
	
	if su.TmpFile == "" {
		tf, err := os.CreateTemp(conf.Conf.TempDir, "file-*")
		if err != nil {
			return fmt.Errorf("CreateTemp error: %w", err)
		}
		
		abspath := tf.Name() //这里返回的是绝对路径
		if err = os.Truncate(abspath, int64(su.Size)); err != nil {
			tf.Close() // 确保文件被关闭
			os.Remove(abspath) // 清理文件
			return fmt.Errorf("Truncate error: %w", err)
		}
		
		su.TmpFile = abspath
		su.tmpFile = tf
		return nil
	}
	
	if su.tmpFile == nil {
		var err error
		su.tmpFile, err = os.OpenFile(su.TmpFile, os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("OpenFile error: %w", err)
		}
	}
	return nil
}

// cleanup 清理资源，线程安全
func (su *sliceup) cleanup() {
	su.Lock()
	defer su.Unlock()
	
	if su.tmpFile != nil {
		if closeErr := su.tmpFile.Close(); closeErr != nil {
			log.Errorf("Failed to close tmp file: %v", closeErr)
		}
		su.tmpFile = nil
	}
	
	if su.TmpFile != "" {
		if removeErr := os.Remove(su.TmpFile); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Errorf("Failed to remove tmp file %s: %v", su.TmpFile, removeErr)
		}
		su.TmpFile = ""
	}
}

// 分片上传缓存，使用TaskID作为key
var sliceupMap = sync.Map{}

type sliceWriter struct {
	file   *os.File
	offset int64
}

// Write implements io.Writer interface
// 虽然每个分片都定义了一个sliceWriter
// 但是Write方法会在同一个分片复制过程中多次调用，
// 所以要更新自身的offset
func (sw *sliceWriter) Write(p []byte) (int, error) {
	n, err := sw.file.WriteAt(p, sw.offset)
	sw.offset += int64(n)
	return n, err
}

// UploadSlice 上传切片，第一个分片必须先上传
func UploadSlice(ctx context.Context, storage driver.Driver, req *reqres.UploadSliceReq, file multipart.File) error {
	var msu *sliceup
	var err error

	// 使用 LoadOrStore 避免并发竞态条件，使用TaskID作为key
	sa, loaded := sliceupMap.LoadOrStore(req.TaskID, nil)
	if !loaded {
		// 首次加载，需要从数据库获取
		su, e := db.GetSliceUploadByTaskID(req.TaskID)
		if e != nil {
			log.Errorf("failed get slice upload [%s]: %+v", req.TaskID, e)
			sliceupMap.Delete(req.TaskID) // 清理无效的 key
			return e
		}
		msu = &sliceup{
			SliceUpload: su,
		}
		sliceupMap.Store(req.TaskID, msu)
	} else {
		msu = sa.(*sliceup)
		// 如果缓存存在，需要刷新数据库状态以确保数据一致性
		if freshSu, err := db.GetSliceUploadByTaskID(req.TaskID); err == nil {
			msu.Lock()
			msu.SliceUpload = freshSu
			msu.Unlock()
		}
	}
	
	// 确保并发安全的错误处理
	defer func() {
		if err != nil {
			msu.Lock()
			msu.Status = tables.SliceUploadStatusFailed
			msu.Message = err.Error()
			updateData := *msu.SliceUpload // 复制数据避免锁持有时间过长
			msu.Unlock()
			
			if updateErr := db.UpdateSliceUpload(&updateData); updateErr != nil {
				log.Errorf("Failed to update slice upload status: %v", updateErr)
			}
		}
	}()

	// 使用锁保护状态检查
	msu.Lock()
	// 检查分片是否已上传过
	if tables.IsSliceUploaded(msu.SliceUploadStatus, int(req.SliceNum)) {
		msu.Unlock()
		log.Warnf("slice already uploaded,req:%+v", req)
		return nil
	}
	msu.Unlock()

	if req.SliceHash != "" {
		msu.Lock()
		sliceHash := []string{} // 分片hash

		//验证分片hash值
		if req.SliceNum == 0 { //第一个分片，slicehash是所有的分片hash
			hs := strings.Split(req.SliceHash, ",")
			if len(hs) != int(msu.SliceCnt) {
				msu.Unlock()
				msg := fmt.Sprintf("failed verify slice hash cnt req: %+v", req)
				log.Error(msg)
				return errors.New(msg)
			}
			// 更新分片hash
			msu.SliceHash = req.SliceHash
			msu.Status = tables.SliceUploadStatusUploading
			updateData := *msu.SliceUpload // 复制数据
			msu.Unlock()
			
			if err := db.UpdateSliceUpload(&updateData); err != nil {
				log.Error("UpdateSliceUpload error", updateData, err)
				return err
			}
			sliceHash = hs
		} else { // 如果不是第一个分片，slicehash是当前分片hash
			sliceHash = strings.Split(msu.SliceHash, ",")
			if len(sliceHash) <= int(req.SliceNum) || req.SliceHash != sliceHash[req.SliceNum] { //比对分片hash是否与之前上传的一致
				msu.Unlock()
				msg := fmt.Sprintf("failed verify slice hash,req: [%+v]", req)
				log.Error(msg)
				return errors.New(msg)
			}
			msu.Unlock()
		}
	}

	switch s := storage.(type) {
	case driver.ISliceUpload:
		if err := s.SliceUpload(ctx, msu.SliceUpload, req.SliceNum, file); err != nil {
			log.Error("SliceUpload error", req, err)
			return err
		}

	default: //其他网盘先缓存到本地
		if err := msu.ensureTmpFile(); err != nil {
			log.Error("ensureTmpFile error", req, err)
			return err
		}

		// 流式复制，减少内存占用
		sw := &sliceWriter{
			file:   msu.tmpFile,
			offset: int64(req.SliceNum) * int64(msu.SliceSize),
		}
		_, err := utils.CopyWithBuffer(sw, file)
		if err != nil {
			log.Error("Copy error", req, err)
			return err
		}
	}
	
	// 原子性更新分片状态
	msu.Lock()
	tables.SetSliceUploaded(msu.SliceUploadStatus, int(req.SliceNum))
	updateData := *msu.SliceUpload // 复制数据
	msu.Unlock()

	err = db.UpdateSliceUpload(&updateData)
	if err != nil {
		log.Error("UpdateSliceUpload error", updateData, err)
		return err
	}
	return nil

}

// SliceUpComplete 完成分片上传
func SliceUpComplete(ctx context.Context, storage driver.Driver, taskID string) (*reqres.UploadSliceCompleteResp, error) {
	var msu *sliceup
	var err error

	sa, ok := sliceupMap.Load(taskID)
	if !ok {
		su, err := db.GetSliceUploadByTaskID(taskID)
		if err != nil {
			log.Errorf("failed get slice upload [%s]: %+v", taskID, err)
			return nil, err
		}
		msu = &sliceup{
			SliceUpload: su,
		}
	} else {
		msu = sa.(*sliceup)
	}
	
	// 检查是否所有分片都已上传
	msu.Lock()
	allUploaded := tables.IsAllSliceUploaded(msu.SliceUploadStatus, msu.SliceCnt)
	msu.Unlock()
	
	if !allUploaded {
		return &reqres.UploadSliceCompleteResp{
			Complete:          0,
			SliceUploadStatus: msu.SliceUploadStatus,
			TaskID:            msu.TaskID,
		}, nil
	}

	defer func() {
		// 确保资源清理和缓存删除
		msu.cleanup()
		sliceupMap.Delete(msu.TaskID)
		
		if err != nil {
			msu.Lock()
			msu.Status = tables.SliceUploadStatusFailed
			msu.Message = err.Error()
			updateData := *msu.SliceUpload
			msu.Unlock()
			
			if updateErr := db.UpdateSliceUpload(&updateData); updateErr != nil {
				log.Errorf("Failed to update slice upload status: %v", updateErr)
			}
		} else {
			// 上传成功后从数据库中删除记录，允许重复上传
			if deleteErr := db.DeleteSliceUploadByTaskID(msu.TaskID); deleteErr != nil {
				log.Errorf("Failed to delete slice upload record: %v", deleteErr)
			}
		}
	}()
	switch s := storage.(type) {
	case driver.IUploadSliceComplete:
		err = s.UploadSliceComplete(ctx, msu.SliceUpload)
		if err != nil {
			log.Error("UploadSliceComplete error", msu.SliceUpload, err)
			return nil, err
		}
		
		msu.Lock()
		msu.Status = tables.SliceUploadStatusComplete
		updateData := *msu.SliceUpload
		msu.Unlock()
		
		if updateErr := db.UpdateSliceUpload(&updateData); updateErr != nil {
			log.Errorf("Failed to update slice upload status to complete: %v", updateErr)
		}
		
		return &reqres.UploadSliceCompleteResp{
			Complete: 1,
			TaskID:   msu.TaskID,
		}, nil

	default:
		// 其他网盘客户端上传到本地后，上传到网盘，使用任务处理
		msu.Lock()
		tmpFile := msu.tmpFile
		msu.Unlock()
		
		if tmpFile == nil {
			err := fmt.Errorf("tmp file not found [%s]", taskID)
			log.Error(err)
			return nil, err
		}
		
		var hashInfo utils.HashInfo
		if msu.HashMd5 != "" {
			hashInfo = utils.NewHashInfo(utils.MD5, msu.HashMd5)
		} else if msu.HashSha1 != "" {
			hashInfo = utils.NewHashInfo(utils.SHA1, msu.HashSha1)
		}

		file := &stream.FileStream{
			Obj: &model.Object{
				Name:     msu.Name,
				Size:     msu.Size,
				Modified: time.Now(),
				HashInfo: hashInfo,
			},
		}
		file.Mimetype = utils.GetMimeType(msu.Name)
		
		if msu.AsTask {
			file.SetTmpFile(tmpFile)
			// 防止defer中清理文件
			msu.Lock()
			msu.tmpFile = nil
			msu.TmpFile = ""
			msu.Unlock()
			
			_, err = putAsTask(ctx, msu.DstPath, file)
			if err != nil {
				log.Error("putAsTask error", msu.SliceUpload, err)
				return nil, err
			}
			return &reqres.UploadSliceCompleteResp{
				Complete: 2,
				TaskID:   msu.TaskID,
			}, nil
		}
		
		file.Reader = tmpFile
		err = op.Put(ctx, storage, msu.ActualPath, file, nil)
		if err != nil {
			log.Error("Put error", msu.SliceUpload, err)
			return nil, err
		}
		return &reqres.UploadSliceCompleteResp{
			Complete: 1,
			TaskID:   msu.TaskID,
		}, nil
	}
}
