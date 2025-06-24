package fs

import (
	"context"
	"fmt"
	stdpath "path"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/internal/driver"
	"github.com/OpenListTeam/OpenList/internal/errs"
	"github.com/OpenListTeam/OpenList/internal/model"
	"github.com/OpenListTeam/OpenList/internal/op"
	"github.com/OpenListTeam/OpenList/internal/task"
	"github.com/OpenListTeam/OpenList/pkg/utils"
	"github.com/pkg/errors"
	"github.com/xhofe/tache"
)

type MoveTask struct {
	task.TaskExtension
	Status       string        `json:"-"`
	SrcObjPath   string        `json:"src_path"`
	DstDirPath   string        `json:"dst_path"`
	srcStorage   driver.Driver `json:"-"`
	dstStorage   driver.Driver `json:"-"`
	SrcStorageMp string        `json:"src_storage_mp"`
	DstStorageMp string        `json:"dst_storage_mp"`
	isSubTask    bool          `json:"-"`
}

func (t *MoveTask) GetName() string {
	return fmt.Sprintf("move [%s](%s) to [%s](%s)", t.SrcStorageMp, t.SrcObjPath, t.DstStorageMp, t.DstDirPath)
}

func (t *MoveTask) GetStatus() string {
	return t.Status
}

func (t *MoveTask) Run() error {
	t.ReinitCtx()
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()

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

	return moveBetween2Storages(t, t.srcStorage, t.dstStorage, t.SrcObjPath, t.DstDirPath)
}

var MoveTaskManager *tache.Manager[*MoveTask]

func moveBetween2Storages(t *MoveTask, srcStorage, dstStorage driver.Driver, srcObjPath, dstDirPath string) error {
	t.Status = "getting src object"
	srcObj, err := op.Get(t.Ctx(), srcStorage, srcObjPath)
	if err != nil {
		return errors.WithMessagef(err, "failed get src [%s] file", srcObjPath)
	}

	if srcObj.IsDir() {
		t.Status = "creating destination directory"
		dstObjPath := stdpath.Join(dstDirPath, srcObj.GetName())
		err = op.MakeDir(t.Ctx(), dstStorage, dstObjPath)
		if err != nil && !errors.Is(err, errs.FileExisted) {
			return errors.WithMessagef(err, "failed to create destination directory [%s]", dstObjPath)
		}

		t.Status = "listing source directory"
		objs, err := op.List(t.Ctx(), srcStorage, srcObjPath, model.ListArgs{})
		if err != nil {
			return errors.WithMessagef(err, "failed to list [%s]", srcObjPath)
		}

		var wg sync.WaitGroup
		errCh := make(chan error, len(objs))
		for _, obj := range objs {
			if utils.IsCanceled(t.Ctx()) {
				return nil
			}

			subTask := &MoveTask{
				TaskExtension: task.TaskExtension{
					Creator: t.GetCreator(),
				},
				srcStorage:   srcStorage,
				dstStorage:   dstStorage,
				SrcObjPath:   stdpath.Join(srcObjPath, obj.GetName()),
				DstDirPath:   dstObjPath,
				SrcStorageMp: srcStorage.GetStorage().MountPath,
				DstStorageMp: dstStorage.GetStorage().MountPath,
				isSubTask:    true,
			}

			wg.Add(1)
			go func(st *MoveTask) {
				defer wg.Done()
				if err := st.Run(); err != nil {
					errCh <- err
				}
			}(subTask)
		}

		wg.Wait()
		close(errCh)
		if len(errCh) > 0 {
			return errors.Errorf("some sub move tasks failed: %v", <-errCh)
		}

		t.Status = "verifying copied structure"
		ok, err := verifyDirStructureCopied(t.Ctx(), srcStorage, dstStorage, srcObjPath, dstObjPath)
		if err != nil {
			return errors.WithMessage(err, "failed to verify copied structure")
		}
		if !ok {
			return errors.New("copy verification failed: directory structure mismatch")
		}

		t.Status = "cleaning up source directory"
		err = op.Remove(t.Ctx(), srcStorage, srcObjPath)
		if err != nil {
			t.Status = "completed (source directory cleanup pending)"
		} else {
			t.Status = "completed"
		}
		return nil
	}

	return moveFileBetween2Storages(t, srcStorage, dstStorage, srcObjPath, dstDirPath)
}

func moveFileBetween2Storages(tsk *MoveTask, srcStorage, dstStorage driver.Driver, srcFilePath, dstDirPath string) error {
	tsk.Status = "copying file to destination"
	copyTask := &CopyTask{
		TaskExtension: task.TaskExtension{
			Creator: tsk.GetCreator(),
		},
		srcStorage:   srcStorage,
		dstStorage:   dstStorage,
		SrcObjPath:   srcFilePath,
		DstDirPath:   dstDirPath,
		SrcStorageMp: srcStorage.GetStorage().MountPath,
		DstStorageMp: dstStorage.GetStorage().MountPath,
	}

	copyTask.SetCtx(tsk.Ctx())
	err := copyBetween2Storages(copyTask, srcStorage, dstStorage, srcFilePath, dstDirPath)
	if err != nil {
		return errors.WithMessagef(err, "failed to copy [%s]", srcFilePath)
	}

	tsk.SetProgress(50)
	tsk.Status = "deleting source file"
	err = op.Remove(tsk.Ctx(), srcStorage, srcFilePath)
	if err != nil {
		return errors.WithMessagef(err, "failed to delete source [%s]", srcFilePath)
	}
	tsk.SetProgress(100)
	tsk.Status = "completed"
	return nil
}

func _move(ctx context.Context, srcObjPath, dstDirPath string, lazyCache ...bool) (task.TaskExtensionInfo, error) {
	srcStorage, srcObjActualPath, err := op.GetStorageAndActualPath(srcObjPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get src storage")
	}
	dstStorage, dstDirActualPath, err := op.GetStorageAndActualPath(dstDirPath)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get dst storage")
	}

	if srcStorage.GetStorage() == dstStorage.GetStorage() {
		err = op.Move(ctx, srcStorage, srcObjActualPath, dstDirActualPath, lazyCache...)
		if !errors.Is(err, errs.NotImplement) && !errors.Is(err, errs.NotSupport) {
			return nil, err
		}
	}

	taskCreator, _ := ctx.Value("user").(*model.User)
	t := &MoveTask{
		TaskExtension: task.TaskExtension{
			Creator: taskCreator,
		},
		srcStorage:   srcStorage,
		dstStorage:   dstStorage,
		SrcObjPath:   srcObjActualPath,
		DstDirPath:   dstDirActualPath,
		SrcStorageMp: srcStorage.GetStorage().MountPath,
		DstStorageMp: dstStorage.GetStorage().MountPath,
	}
	MoveTaskManager.Add(t)
	return t, nil
}

// verifyDirStructureCopied 对比源和目标存储的路径下结构是否一致（名称、是否文件）
func verifyDirStructureCopied(ctx context.Context, srcStorage, dstStorage driver.Driver, srcPath, dstPath string) (bool, error) {
	srcObjs, err := op.List(ctx, srcStorage, srcPath, model.ListArgs{})
	if err != nil {
		return false, err
	}
	dstObjs, err := op.List(ctx, dstStorage, dstPath, model.ListArgs{})
	if err != nil {
		return false, err
	}

	if len(srcObjs) != len(dstObjs) {
		return false, nil
	}

	srcMap := make(map[string]bool)
	for _, obj := range srcObjs {
		srcMap[obj.GetName()] = obj.IsDir()
	}
	for _, obj := range dstObjs {
		isDir, exists := srcMap[obj.GetName()]
		if !exists || isDir != obj.IsDir() {
			return false, nil
		}
	}
	return true, nil
}
