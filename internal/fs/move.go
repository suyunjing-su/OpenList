package fs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/internal/conf"
	"github.com/OpenListTeam/OpenList/internal/driver"
	"github.com/OpenListTeam/OpenList/internal/model"
	"github.com/OpenListTeam/OpenList/internal/op"
	"github.com/OpenListTeam/OpenList/internal/task"
	"github.com/xhofe/tache"
)

type MoveTask struct {
	task.TaskExtension
	Status       string
	Progress     int64
	Total        int64
	SrcObjs      []model.Obj
	DstDir       string
	SrcStorageMp string
	DstStorageMp string
	srcStorage   driver.Driver
	dstStorage   driver.Driver
	mu           sync.Mutex
}

func (t *MoveTask) GetName() string {
	return fmt.Sprintf("move [%s] to [%s]", t.SrcStorageMp, t.DstStorageMp)
}
func (t *MoveTask) GetStatus() string {
	return t.Status
}
func (t *MoveTask) GetProgress() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Total == 0 {
		return 0
	}
	return float64(t.Progress) / float64(t.Total)
}

func (t *MoveTask) Run() error {
	t.ReinitCtx()
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()

	if t.srcStorage == nil {
		var err error
		t.srcStorage, err = op.GetStorageByMountPath(t.SrcStorageMp)
		if err != nil {
			return err
		}
	}
	if t.dstStorage == nil {
		var err error
		t.dstStorage, err = op.GetStorageByMountPath(t.DstStorageMp)
		if err != nil {
			return err
		}
	}

	t.Total = int64(len(t.SrcObjs))

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, obj := range t.SrcObjs {
		wg.Add(1)
		go func(obj model.Obj) {
			defer wg.Done()
			srcPath := obj.GetPath()
			dstPath := strings.TrimSuffix(t.DstDir, "/") + "/" + strings.TrimPrefix(srcPath, strings.TrimSuffix(t.SrcObjs[0].GetPath(), obj.GetName()))

			copyTask := &CopyTask{
				TaskExtension: task.TaskExtension{
					Creator: t.GetCreator(),
				},
				srcStorage:   t.srcStorage,
				dstStorage:   t.dstStorage,
				SrcObjPath:   srcPath,
				DstDirPath:   dstPath,
				SrcStorageMp: t.SrcStorageMp,
				DstStorageMp: t.DstStorageMp,
			}
			if err := copyTask.Run(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			t.mu.Lock()
			t.Progress++
			t.mu.Unlock()
		}(obj)
	}
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("partial move failed: %v", errs)
	}

	for _, obj := range t.SrcObjs {
		_ = op.Delete(t.Ctx(), t.srcStorage, obj.GetPath())
	}

	t.Status = "done"
	return nil
}

var MoveTaskManager = tache.NewManager[*MoveTask]()

func GetMoveProgress(taskID string) (float64, string) {
	t := MoveTaskManager.Get(taskID)
	if t == nil {
		return 0, "not found"
	}
	return t.GetProgress(), t.GetStatus()
}

func Move(ctx context.Context, srcPaths []string, dstPath string) (task.TaskExtensionInfo, error) {
	if len(srcPaths) == 0 {
		return nil, errors.New("empty source")
	}
	srcStorage, firstPath, err := op.GetStorageAndActualPath(srcPaths[0])
	if err != nil {
		return nil, err
	}
	dstStorage, dstActualPath, err := op.GetStorageAndActualPath(dstPath)
	if err != nil {
		return nil, err
	}

	if srcStorage.GetStorage() == dstStorage.GetStorage() {
		for _, p := range srcPaths {
			if err := op.Move(ctx, srcStorage, p, dstActualPath); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if ctx.Value(conf.NoTaskKey) != nil {
		return nil, errors.New("跨存储移动需异步处理")
	}

	var objs []model.Obj
	for _, path := range srcPaths {
		st, ap, err := op.GetStorageAndActualPath(path)
		if err != nil {
			return nil, err
		}
		obj, err := op.Get(ctx, st, ap)
		if err != nil {
			return nil, err
		}
		objs = append(objs, obj)
	}

	taskCreator, _ := ctx.Value("user").(*model.User)
	t := &MoveTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
		},
		SrcObjs:      objs,
		DstDir:       dstActualPath,
		SrcStorageMp: srcStorage.GetStorage().MountPath,
		DstStorageMp: dstStorage.GetStorage().MountPath,
		srcStorage:   srcStorage,
		dstStorage:   dstStorage,
	}
	MoveTaskManager.Add(t)
	return t, nil
}

func _move(ctx context.Context, src, dst string) error {
	_, err := Move(ctx, []string{src}, dst)
	return err
}
