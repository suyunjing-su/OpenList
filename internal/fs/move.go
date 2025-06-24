package fs

import (
	"context"
	"net/http"
	"path"
	"sync"

	"github.com/OpenListTeam/OpenList/internal/conf"
	"github.com/OpenListTeam/OpenList/internal/driver"
	"github.com/OpenListTeam/OpenList/internal/errs"
	"github.com/OpenListTeam/OpenList/internal/model"
	"github.com/OpenListTeam/OpenList/internal/op"
	"github.com/OpenListTeam/OpenList/internal/task"
	"github.com/OpenListTeam/OpenList/pkg/utils"
	"github.com/pkg/errors"
)

type MoveDirTask struct {
	task.TaskExtension
	SrcStorage driver.Driver
	DstStorage driver.Driver
	SrcPath    string
	DstPath    string
	SubTasks   []*CopyTask
	Progress   float64
	Status     string
	Lock       sync.Mutex
	DoneChan   chan struct{}
}

func (t *MoveDirTask) GetName() string {
	return "Move directory with verification"
}

func (t *MoveDirTask) GetStatus() string {
	t.Lock.Lock()
	defer t.Lock.Unlock()
	return t.Status
}

func (t *MoveDirTask) Run() error {
	t.ReinitCtx()
	t.DoneChan = make(chan struct{})
	t.Status = "Listing source directory"

	srcList, err := recursiveList(t.Ctx(), t.SrcStorage, t.SrcPath)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, obj := range srcList {
		if utils.IsCanceled(t.Ctx()) {
			return nil
		}
		copyTask := &CopyTask{
			TaskExtension: task.TaskExtension{
				Creator: t.Creator,
			},
			srcStorage:   t.SrcStorage,
			dstStorage:   t.DstStorage,
			SrcObjPath:   obj.Path,
			DstDirPath:   path.Join(t.DstPath, utils.TrimPrefix(obj.Path, t.SrcPath)),
			SrcStorageMp: t.SrcStorage.GetStorage().MountPath,
			DstStorageMp: t.DstStorage.GetStorage().MountPath,
		}
		t.SubTasks = append(t.SubTasks, copyTask)
		wg.Add(1)
		go func(ct *CopyTask) {
			defer wg.Done()
			_ = ct.Run() // Ignore internal error handling for simplicity
		} (copyTask)
	}

	wg.Wait()
	t.Status = "Verifying copied directory"
	dstList, err := recursiveList(t.Ctx(), t.DstStorage, t.DstPath)
	if err != nil {
		return err
	}
	if !comparePathList(srcList, dstList) {
		return errors.New("target verification failed: destination does not match source")
	}

	t.Status = "Deleting source files"
	for i := len(srcList) - 1; i >= 0; i-- {
		_ = op.Delete(t.Ctx(), t.SrcStorage, srcList[i].Path)
	}
	t.Status = "Completed"
	close(t.DoneChan)
	return nil
}

func recursiveList(ctx context.Context, drv driver.Driver, base string) ([]*model.Obj, error) {
	objs := []*model.Obj{}
	list, err := op.List(ctx, drv, base, model.ListArgs{})
	if err != nil {
		return nil, err
	}
	for _, obj := range list {
		fullPath := path.Join(base, obj.GetName())
		obj.SetPath(fullPath)
		objs = append(objs, obj)
		if obj.IsDir() {
			subObjs, err := recursiveList(ctx, drv, fullPath)
			if err != nil {
				return nil, err
			}
			objs = append(objs, subObjs...)
		}
	}
	return objs, nil
}

func comparePathList(src, dst []*model.Obj) bool {
	if len(src) != len(dst) {
		return false
	}
	m := map[string]bool{}
	for _, obj := range dst {
		m[obj.GetPath()+"-"+obj.GetType()] = true
	}
	for _, obj := range src {
		if !m[obj.GetPath()+"-"+obj.GetType()] {
			return false
		}
	}
	return true
}

// 发起移动任务（跨存储目录）
func MoveWithVerify(ctx context.Context, srcPath, dstPath string) (task.TaskExtensionInfo, error) {
	srcStorage, srcActualPath, err := op.GetStorageAndActualPath(srcPath)
	if err != nil {
		return nil, err
	}
	dstStorage, dstActualPath, err := op.GetStorageAndActualPath(dstPath)
	if err != nil {
		return nil, err
	}

	srcObj, err := op.Get(ctx, srcStorage, srcActualPath)
	if err != nil {
		return nil, err
	}

	if srcStorage.GetStorage() == dstStorage.GetStorage() || !srcObj.IsDir() {
		return op.Move(ctx, srcStorage, srcActualPath, dstActualPath)
	}

	if ctx.Value(conf.NoTaskKey) != nil {
		return nil, errs.NotSupport
	}

	taskCreator, _ := ctx.Value("user").(*model.User)
	t := &MoveDirTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
		},
		SrcStorage: srcStorage,
		DstStorage: dstStorage,
		SrcPath:    srcActualPath,
		DstPath:    dstActualPath,
	}
	task.GlobalTaskManager.Add(t)
	return t, nil
}

// move task API Service
func GetMoveProgress(taskID string) (float64, string) {
	v := task.GlobalTaskManager.Get(taskID)
	if mv, ok := v.(*MoveDirTask); ok {
		return mv.GetProgress(), mv.GetStatus()
	}
	return 0, "not found"
}

func (t *MoveDirTask) GetProgress() float64 {
	t.Lock.Lock()
	defer t.Lock.Unlock()
	var total, done int64
	for _, ct := range t.SubTasks {
		total += ct.GetTotalBytes()
		done += ct.GetCompletedBytes()
	}
	if total == 0 {
		return 0
	}
	return float64(done) / float64(total)
}