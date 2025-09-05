package fs

import (
	"context"
	"fmt"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/model/reqres"
	"github.com/OpenListTeam/OpenList/v4/internal/model/tables"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/pkg/tempdir"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	log "github.com/sirupsen/logrus"
)

// SliceUploadManager 分片上传管理器
type SliceUploadManager struct {
	cache    sync.Map         // TaskID -> *SliceUploadSession
	tempDir  string          // 临时文件目录
}

// SliceUploadSession 分片上传会话
type SliceUploadSession struct {
	*tables.SliceUpload
	tmpFile *os.File
	mutex   sync.Mutex // 使用Mutex而不是RWMutex，保持与原始实现一致
}

// NewSliceUploadManager 创建分片上传管理器
func NewSliceUploadManager() *SliceUploadManager {
	tempDirPath := tempdir.GetPersistentTempDir()
	return &SliceUploadManager{
		tempDir: tempDirPath,
	}
}

// CreateSession 创建新的上传会话 - 完整实现Preup逻辑
func (m *SliceUploadManager) CreateSession(ctx context.Context, storage driver.Driver, actualPath string, req *reqres.PreupReq) (*reqres.PreupResp, error) {
	// 检查是否存在未完成的上传任务（用于断点续传）
	wh := map[string]any{
		"dst_path": req.Path,
		"name":     req.Name,
		"size":     req.Size,
		"status":   tables.SliceUploadStatusUploading, // 只查找正在进行中的任务
	}
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
		// 验证临时文件是否仍然存在（重启后可能被清理）
		if su.TmpFile != "" {
			if _, err := os.Stat(su.TmpFile); os.IsNotExist(err) {
				// 临时文件丢失，清理数据库记录，重新开始
				log.Warnf("Temporary file lost after restart, cleaning up task: %s", su.TaskID)
				if deleteErr := db.DeleteSliceUploadByTaskID(su.TaskID); deleteErr != nil {
					log.Errorf("Failed to delete lost slice upload task: %v", deleteErr)
				}
				// 继续创建新任务
			} else {
				// 临时文件存在，可以继续断点续传
				session := &SliceUploadSession{SliceUpload: su}
				m.cache.Store(su.TaskID, session)
				log.Infof("Resuming slice upload after restart: %s, completed slices: %d/%d", 
					su.TaskID, tables.CountUploadedSlices(su.SliceUploadStatus), su.SliceCnt)
				return &reqres.PreupResp{
					TaskID:            su.TaskID,
					SliceSize:         su.SliceSize,
					SliceCnt:          su.SliceCnt,
					SliceUploadStatus: su.SliceUploadStatus,
				}, nil
			}
		} else {
			// 原生分片上传（如123open/baidu），无需临时文件
			session := &SliceUploadSession{SliceUpload: su}
			m.cache.Store(su.TaskID, session)
			log.Infof("Resuming native slice upload after restart: %s", su.TaskID)
			return &reqres.PreupResp{
				TaskID:            su.TaskID,
				SliceSize:         su.SliceSize,
				SliceCnt:          su.SliceCnt,
				SliceUploadStatus: su.SliceUploadStatus,
			}, nil
		}
	}
	
	srcobj, err := op.Get(ctx, storage, actualPath)
	if err != nil {
		log.Error(err)
		return nil, errors.WithStack(err)
	}
	user, _ := ctx.Value(conf.UserKey).(*model.User)

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
		AsTask:       req.AsTask,
	}
	if user != nil {
		createsu.UserID = user.ID
	}
	log.Infof("storage mount path %s", storage.GetStorage().MountPath)
	
	switch st := storage.(type) {
	case driver.IPreup:
		log.Info("preup support")
		res, err := st.Preup(ctx, srcobj, req)
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
	
	session := &SliceUploadSession{SliceUpload: createsu}
	m.cache.Store(taskID, session)
	
	return &reqres.PreupResp{
		Reuse:             false,
		SliceUploadStatus: createsu.SliceUploadStatus,
		SliceSize:         createsu.SliceSize,
		SliceCnt:          createsu.SliceCnt,
		TaskID:            createsu.TaskID,
	}, nil
}

// getOrLoadSession 获取或加载会话，提高代码复用性
func (m *SliceUploadManager) getOrLoadSession(taskID string) (*SliceUploadSession, error) {
	sa, loaded := m.cache.LoadOrStore(taskID, (*SliceUploadSession)(nil))
	if !loaded {
		// 首次加载，需要从数据库获取
		su, err := db.GetSliceUploadByTaskID(taskID)
		if err != nil {
			m.cache.Delete(taskID) // 清理无效的 key
			return nil, errors.WithMessagef(err, "failed get slice upload [%s]", taskID)
		}
		session := &SliceUploadSession{
			SliceUpload: su,
		}
		m.cache.Store(taskID, session)
		return session, nil
	}
	
	// 缓存中存在，但可能是nil值，需要检查
	if sa == nil {
		// 说明之前存储了nil，需要重新从数据库加载
		su, err := db.GetSliceUploadByTaskID(taskID)
		if err != nil {
			m.cache.Delete(taskID)
			return nil, errors.WithMessagef(err, "failed get slice upload [%s]", taskID)
		}
		session := &SliceUploadSession{
			SliceUpload: su,
		}
		m.cache.Store(taskID, session)
		return session, nil
	}
	
	session := sa.(*SliceUploadSession)
	// 刷新数据库状态以确保数据一致性
	if freshSu, err := db.GetSliceUploadByTaskID(taskID); err == nil {
		session.mutex.Lock()
		session.SliceUpload = freshSu
		session.mutex.Unlock()
	}
	return session, nil
}

// UploadSlice 上传分片 - 完整实现原始逻辑
func (m *SliceUploadManager) UploadSlice(ctx context.Context, storage driver.Driver, req *reqres.UploadSliceReq, file multipart.File) error {
	var err error

	session, err := m.getOrLoadSession(req.TaskID)
	if err != nil {
		log.Errorf("failed to get session: %+v", err)
		return err
	}
	
	// 确保并发安全的错误处理
	defer func() {
		if err != nil {
			session.mutex.Lock()
			session.Status = tables.SliceUploadStatusFailed
			session.Message = err.Error()
			updateData := *session.SliceUpload // 复制数据避免锁持有时间过长
			session.mutex.Unlock()
			
			if updateErr := db.UpdateSliceUpload(&updateData); updateErr != nil {
				log.Errorf("Failed to update slice upload status: %v", updateErr)
			}
		}
	}()

	// 使用锁保护状态检查
	session.mutex.Lock()
	// 检查分片是否已上传过
	if tables.IsSliceUploaded(session.SliceUploadStatus, int(req.SliceNum)) {
		session.mutex.Unlock()
		log.Warnf("slice already uploaded,req:%+v", req)
		return nil
	}
	session.mutex.Unlock()

	// 分片hash验证逻辑
	if req.SliceHash != "" {
		session.mutex.Lock()

		//验证分片hash值
		if req.SliceNum == 0 { //第一个分片，slicehash是所有的分片hash
			hs := strings.Split(req.SliceHash, ",")
			if len(hs) != int(session.SliceCnt) {
				session.mutex.Unlock()
				err := fmt.Errorf("slice hash count mismatch, expected %d, got %d", session.SliceCnt, len(hs))
				log.Error("slice hash count mismatch", req, err)
				return err
			}
			session.SliceHash = req.SliceHash // 存储完整的hash字符串
		} else {
			session.SliceHash = req.SliceHash // 存储单个分片hash
		}
		session.mutex.Unlock()
	}

	// 根据存储类型处理分片上传
	switch s := storage.(type) {
	case driver.ISliceUpload:
		log.Info("SliceUpload support")
		if err := s.SliceUpload(ctx, session.SliceUpload, req.SliceNum, file); err != nil {
			log.Error("SliceUpload error", req, err)
			return err
		}

	default: //其他网盘先缓存到本地
		if err := session.ensureTmpFile(); err != nil {
			log.Error("ensureTmpFile error", req, err)
			return err
		}

		// 流式复制，减少内存占用
		sw := &sliceWriter{
			file:   session.tmpFile,
			offset: int64(req.SliceNum) * int64(session.SliceSize),
		}
		_, err := utils.CopyWithBuffer(sw, file)
		if err != nil {
			log.Error("Copy error", req, err)
			return err
		}
	}
	
	// 原子性更新分片状态
	session.mutex.Lock()
	tables.SetSliceUploaded(session.SliceUploadStatus, int(req.SliceNum))
	updateData := *session.SliceUpload // 复制数据
	session.mutex.Unlock()

	err = db.UpdateSliceUpload(&updateData)
	if err != nil {
		log.Error("UpdateSliceUpload error", updateData, err)
		return err
	}
	return nil
}

// CompleteUpload 完成上传 - 完整实现原始逻辑
func (m *SliceUploadManager) CompleteUpload(ctx context.Context, storage driver.Driver, taskID string) (*reqres.UploadSliceCompleteResp, error) {
	var err error

	session, err := m.getOrLoadSession(taskID)
	if err != nil {
		log.Errorf("failed to get session: %+v", err)
		return nil, err
	}
	
	// 检查是否所有分片都已上传
	session.mutex.Lock()
	allUploaded := tables.IsAllSliceUploaded(session.SliceUploadStatus, session.SliceCnt)
	session.mutex.Unlock()
	
	if !allUploaded {
		return &reqres.UploadSliceCompleteResp{
			Complete:          0,
			SliceUploadStatus: session.SliceUploadStatus,
			TaskID:            session.TaskID,
		}, nil
	}

	defer func() {
		// 确保资源清理和缓存删除
		session.cleanup()
		m.cache.Delete(session.TaskID)
		
		if err != nil {
			session.mutex.Lock()
			session.Status = tables.SliceUploadStatusFailed
			session.Message = err.Error()
			updateData := *session.SliceUpload
			session.mutex.Unlock()
			
			if updateErr := db.UpdateSliceUpload(&updateData); updateErr != nil {
				log.Errorf("Failed to update slice upload status: %v", updateErr)
			}
		} else {
			// 上传成功后从数据库中删除记录，允许重复上传
			if deleteErr := db.DeleteSliceUploadByTaskID(session.TaskID); deleteErr != nil {
				log.Errorf("Failed to delete slice upload record: %v", deleteErr)
			}
		}
	}()
	
	switch s := storage.(type) {
	case driver.IUploadSliceComplete:
		err = s.UploadSliceComplete(ctx, session.SliceUpload)
		if err != nil {
			log.Error("UploadSliceComplete error", session.SliceUpload, err)
			return nil, err
		}
		
		// 原生分片上传成功，直接返回，defer中会删除数据库记录
		return &reqres.UploadSliceCompleteResp{
			Complete: 1,
			TaskID:   session.TaskID,
		}, nil

	default:
		// 其他网盘客户端上传到本地后，上传到网盘，使用任务处理
		session.mutex.Lock()
		tmpFile := session.tmpFile
		session.mutex.Unlock()
		
		if tmpFile == nil {
			err := fmt.Errorf("tmp file not found [%s]", taskID)
			log.Error(err)
			return nil, err
		}
		
		var hashInfo utils.HashInfo
		if session.HashMd5 != "" {
			hashInfo = utils.NewHashInfo(utils.MD5, session.HashMd5)
		} else if session.HashSha1 != "" {
			hashInfo = utils.NewHashInfo(utils.SHA1, session.HashSha1)
		}

		file := &stream.FileStream{
			Obj: &model.Object{
				Name:     session.Name,
				Size:     session.Size,
				Modified: time.Now(),
				HashInfo: hashInfo,
			},
		}
		file.Mimetype = utils.GetMimeType(session.Name)
		
		if session.AsTask {
			file.SetTmpFile(tmpFile)
			// 防止defer中清理文件
			session.mutex.Lock()
			session.tmpFile = nil
			session.TmpFile = ""
			session.mutex.Unlock()
			
			_, err = putAsTask(ctx, session.DstPath, file)
			if err != nil {
				log.Error("putAsTask error", session.SliceUpload, err)
				return nil, err
			}
			return &reqres.UploadSliceCompleteResp{
				Complete: 2,
				TaskID:   session.TaskID,
			}, nil
		}
		
		file.Reader = tmpFile
		err = op.Put(ctx, storage, session.ActualPath, file, nil)
		if err != nil {
			log.Error("Put error", session.SliceUpload, err)
			return nil, err
		}
		return &reqres.UploadSliceCompleteResp{
			Complete: 1,
			TaskID:   session.TaskID,
		}, nil
	}
}

// ensureTmpFile 确保临时文件存在且正确初始化，线程安全 - 使用持久化目录
func (s *SliceUploadSession) ensureTmpFile() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if s.TmpFile == "" {
		tempDirPath := tempdir.GetPersistentTempDir()
		
		// 使用TaskID作为文件名的一部分，确保唯一性和可识别性
		filename := fmt.Sprintf("slice_upload_%s_%s", s.TaskID, s.Name)
		// 清理文件名中的特殊字符
		filename = strings.ReplaceAll(filename, "/", "_")
		filename = strings.ReplaceAll(filename, "\\", "_")
		filename = strings.ReplaceAll(filename, ":", "_")
		
		tmpPath := filepath.Join(tempDirPath, filename)
		
		tf, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("create persistent temp file error: %w", err)
		}
		
		if err = os.Truncate(tmpPath, int64(s.Size)); err != nil {
			tf.Close() // 确保文件被关闭
			os.Remove(tmpPath) // 清理文件
			return fmt.Errorf("truncate persistent temp file error: %w", err)
		}
		
		s.TmpFile = tmpPath
		s.tmpFile = tf
		
		log.Debugf("Created persistent temp file: %s", tmpPath)
		return nil
	}
	
	if s.tmpFile == nil {
		var err error
		s.tmpFile, err = os.OpenFile(s.TmpFile, os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("reopen persistent temp file error: %w", err)
		}
		log.Debugf("Reopened persistent temp file: %s", s.TmpFile)
	}
	return nil
}

// cleanup 清理资源，线程安全 - 保持原始实现
func (s *SliceUploadSession) cleanup() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if s.tmpFile != nil {
		if closeErr := s.tmpFile.Close(); closeErr != nil {
			log.Errorf("Failed to close tmp file: %v", closeErr)
		}
		s.tmpFile = nil
	}
	
	if s.TmpFile != "" {
		if removeErr := os.Remove(s.TmpFile); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Errorf("Failed to remove tmp file %s: %v", s.TmpFile, removeErr)
		}
		s.TmpFile = ""
	}
}

// 全局管理器实例使用延迟初始化
var globalSliceManager *SliceUploadManager
var globalSliceManagerOnce sync.Once

// getGlobalSliceManager 获取全局分片上传管理器（延迟初始化）
func getGlobalSliceManager() *SliceUploadManager {
	globalSliceManagerOnce.Do(func() {
		globalSliceManager = NewSliceUploadManager()
	})
	return globalSliceManager
}

// sliceWriter 分片写入器 - 保持原始实现
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
