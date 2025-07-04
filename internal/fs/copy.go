package fs

import (
	"context"
	"fmt"
	"net/http"
	stdpath "path"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/task"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/OpenListTeam/tache"
	"github.com/google/uuid"
	"github.com/pkg/errors"
)

type CopyTask struct {
	task.TaskExtension
	Status       string        `json:"-"` //don't save status to save space
	SrcObjPath   string        `json:"src_path"`
	DstDirPath   string        `json:"dst_path"`
	srcStorage   driver.Driver `json:"-"`
	dstStorage   driver.Driver `json:"-"`
	SrcStorageMp string        `json:"src_storage_mp"`
	DstStorageMp string        `json:"dst_storage_mp"`
}

func (t *CopyTask) GetName() string {
	return fmt.Sprintf("copy [%s](%s) to [%s](%s)", t.SrcStorageMp, t.SrcObjPath, t.DstStorageMp, t.DstDirPath)
}

func (t *CopyTask) GetStatus() string {
	return t.Status
}

func (t *CopyTask) Run() error {
	if err := t.ReinitCtx(); err != nil {
		return err
	}
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()
	
	// 注册任务到批量跟踪器
	taskID := t.GetID()
	if taskID == "" {
		taskID = uuid.NewString()
	}
	
	var err error
	if t.srcStorage == nil {
		t.srcStorage, err = op.GetStorageByMountPath(t.SrcStorageMp)
	}
	if t.dstStorage == nil {
		t.dstStorage, err = op.GetStorageByMountPath(t.DstStorageMp)
	}
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	
	// 注册任务到批量跟踪器
	batchTracker.registerTask(taskID, t.dstStorage, t.DstDirPath)
	
	// 执行复制操作
	err = copyBetween2Storages(t, t.srcStorage, t.dstStorage, t.SrcObjPath, t.DstDirPath)
	
	// 标记任务完成并检查是否需要刷新缓存
	if err == nil {
		shouldRefresh, dstStorage, dstDirPath := batchTracker.markTaskCompleted(taskID)
		if shouldRefresh {
			op.ClearCache(dstStorage, dstDirPath)
		}
	} else {
		// 即使失败也要清理跟踪器中的记录
		batchTracker.markTaskCompleted(taskID)
	}
	
	return err
}

var CopyTaskManager *tache.Manager[*CopyTask]

// 批量复制任务跟踪器 - 按目标目录聚合所有复制任务
type batchCopyTracker struct {
	mu           sync.Mutex
	dirTasks     map[string]*dirTaskInfo // dstStoragePath+dstDirPath -> dirTaskInfo
	pendingTasks map[string]string       // taskID -> dstStoragePath+dstDirPath
}

type dirTaskInfo struct {
	dstStorage     driver.Driver
	dstDirPath     string
	pendingTasks   map[string]bool // taskID -> true
	lastActivity   time.Time       // 最后活动时间
}

var batchTracker = &batchCopyTracker{
	dirTasks:     make(map[string]*dirTaskInfo),
	pendingTasks: make(map[string]string),
}

// 生成目标目录的唯一键
func (bt *batchCopyTracker) getDirKey(dstStorage driver.Driver, dstDirPath string) string {
	return dstStorage.GetStorage().MountPath + ":" + dstDirPath
}

// 注册复制任务到目标目录
func (bt *batchCopyTracker) registerTask(taskID string, dstStorage driver.Driver, dstDirPath string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	
	dirKey := bt.getDirKey(dstStorage, dstDirPath)
	
	// 记录任务到目录的映射
	bt.pendingTasks[taskID] = dirKey
	
	// 初始化或更新目录任务信息
	if info, exists := bt.dirTasks[dirKey]; exists {
		info.pendingTasks[taskID] = true
		info.lastActivity = time.Now()
	} else {
		bt.dirTasks[dirKey] = &dirTaskInfo{
			dstStorage:   dstStorage,
			dstDirPath:   dstDirPath,
			pendingTasks: map[string]bool{taskID: true},
			lastActivity: time.Now(),
		}
	}
}

// 标记任务完成，返回是否需要刷新缓存
func (bt *batchCopyTracker) markTaskCompleted(taskID string) (bool, driver.Driver, string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	
	dirKey, exists := bt.pendingTasks[taskID]
	if !exists {
		return false, nil, ""
	}
	
	// 从待处理任务中移除
	delete(bt.pendingTasks, taskID)
	
	info, exists := bt.dirTasks[dirKey]
	if !exists {
		return false, nil, ""
	}
	
	// 从目录任务中移除
	delete(info.pendingTasks, taskID)
	info.lastActivity = time.Now()
	
	// 如果该目录下没有待处理的任务了，触发缓存刷新
	if len(info.pendingTasks) == 0 {
		dstStorage := info.dstStorage
		dstDirPath := info.dstDirPath
		delete(bt.dirTasks, dirKey)
		return true, dstStorage, dstDirPath
	}
	
	return false, nil, ""
}

// 清理超时的任务（可选的清理机制，防止内存泄漏）
func (bt *batchCopyTracker) cleanupStaleEntries() {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	
	now := time.Now()
	for dirKey, info := range bt.dirTasks {
		// 如果超过1小时没有活动，清理该条目
		if now.Sub(info.lastActivity) > time.Hour {
			// 清理相关的待处理任务
			for taskID := range info.pendingTasks {
				delete(bt.pendingTasks, taskID)
			}
			delete(bt.dirTasks, dirKey)
		}
	}
}

// Copy if in the same storage, call move method
// if not, add copy task
func _copy(ctx context.Context, srcObjPath, dstDirPath string, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	srcStorage, srcObjActualPath, err := op.GetStorageAndActualPath(srcObjPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get src storage")
	}
	dstStorage, dstDirActualPath, err := op.GetStorageAndActualPath(dstDirPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get dst storage")
	}
	// copy if in the same storage, just call driver.Copy
	if srcStorage.GetStorage() == dstStorage.GetStorage() {
		err = op.Copy(ctx, srcStorage, srcObjActualPath, dstDirActualPath, lazyCache...)
		if !errors.Is(err, errs.NotImplement) && !errors.Is(err, errs.NotSupport) {
			if err == nil {
				// 同存储复制成功后刷新目标目录缓存
				op.ClearCache(dstStorage, dstDirActualPath)
			}
			return nil, err
		}
	}
	if ctx.Value(conf.NoTaskKey) != nil {
		srcObj, err := op.Get(ctx, srcStorage, srcObjActualPath)
		if err != nil {
			return nil, errors.WithMessagef(err, "failed get src [%s] file", srcObjPath)
		}
		if !srcObj.IsDir() {
			// copy file directly
			link, _, err := op.Link(ctx, srcStorage, srcObjActualPath, model.LinkArgs{
				Header: http.Header{},
			})
			if err != nil {
				return nil, errors.WithMessagef(err, "failed get [%s] link", srcObjPath)
			}
			fs := stream.FileStream{
				Obj: srcObj,
				Ctx: ctx,
			}
			// any link provided is seekable
			ss, err := stream.NewSeekableStream(fs, link)
			if err != nil {
				return nil, errors.WithMessagef(err, "failed get [%s] stream", srcObjPath)
			}
			err = op.Put(ctx, dstStorage, dstDirActualPath, ss, nil, false)
			if err == nil {
				// 直接文件复制成功后刷新目标目录缓存
				op.ClearCache(dstStorage, dstDirActualPath)
			}
			return nil, err
		}
	}
	// not in the same storage
	taskCreator, _ := ctx.Value("user").(*model.User)
	t := &CopyTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
			ApiUrl:  common.GetApiUrl(ctx),
		},
		srcStorage:   srcStorage,
		dstStorage:   dstStorage,
		SrcObjPath:   srcObjActualPath,
		DstDirPath:   dstDirActualPath,
		SrcStorageMp: srcStorage.GetStorage().MountPath,
		DstStorageMp: dstStorage.GetStorage().MountPath,
	}
	CopyTaskManager.Add(t)
	return t, nil
}

func copyBetween2Storages(t *CopyTask, srcStorage, dstStorage driver.Driver, srcObjPath, dstDirPath string) error {
	t.Status = "getting src object"
	srcObj, err := op.Get(t.Ctx(), srcStorage, srcObjPath)
	if err != nil {
		return errors.WithMessagef(err, "failed get src [%s] file", srcObjPath)
	}
	if srcObj.IsDir() {
		t.Status = "src object is dir, listing objs"
		objs, err := op.List(t.Ctx(), srcStorage, srcObjPath, model.ListArgs{})
		if err != nil {
			return errors.WithMessagef(err, "failed list src [%s] objs", srcObjPath)
		}
		
		for _, obj := range objs {
			if utils.IsCanceled(t.Ctx()) {
				return nil
			}
			srcObjPath := stdpath.Join(srcObjPath, obj.GetName())
			dstObjPath := stdpath.Join(dstDirPath, srcObj.GetName())
			CopyTaskManager.Add(&CopyTask{
				TaskExtension: task.TaskExtension{
					Creator: t.GetCreator(),
					ApiUrl:  t.ApiUrl,
				},
				srcStorage:   srcStorage,
				dstStorage:   dstStorage,
				SrcObjPath:   srcObjPath,
				DstDirPath:   dstObjPath,
				SrcStorageMp: srcStorage.GetStorage().MountPath,
				DstStorageMp: dstStorage.GetStorage().MountPath,
			})
		}
		t.Status = "src object is dir, added all copy tasks of objs"
		return nil
	}
	return copyFileBetween2Storages(t, srcStorage, dstStorage, srcObjPath, dstDirPath)
}

func copyFileBetween2Storages(tsk *CopyTask, srcStorage, dstStorage driver.Driver, srcFilePath, dstDirPath string) error {
	srcFile, err := op.Get(tsk.Ctx(), srcStorage, srcFilePath)
	if err != nil {
		return errors.WithMessagef(err, "failed get src [%s] file", srcFilePath)
	}
	tsk.SetTotalBytes(srcFile.GetSize())
	link, _, err := op.Link(tsk.Ctx(), srcStorage, srcFilePath, model.LinkArgs{
		Header: http.Header{},
	})
	if err != nil {
		return errors.WithMessagef(err, "failed get [%s] link", srcFilePath)
	}
	fs := stream.FileStream{
		Obj: srcFile,
		Ctx: tsk.Ctx(),
	}
	// any link provided is seekable
	ss, err := stream.NewSeekableStream(fs, link)
	if err != nil {
		return errors.WithMessagef(err, "failed get [%s] stream", srcFilePath)
	}
	return op.Put(tsk.Ctx(), dstStorage, dstDirPath, ss, tsk.SetProgress, true)
}