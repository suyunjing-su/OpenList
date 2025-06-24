package fs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/internal/conf"
	"github.com/OpenListTeam/OpenList/internal/driver"
	"github.com/OpenListTeam/OpenList/internal/model"
	"github.com/OpenListTeam/OpenList/internal/op"
	"github.com/OpenListTeam/OpenList/internal/stream"
	"github.com/OpenListTeam/OpenList/internal/task"
	"github.com/xhofe/tache"
)

type MoveTask struct {
	task.TaskExtension
	SrcStorage   driver.Driver
	DstStorage   driver.Driver
	SrcPath      string
	DstPath      string
	SrcStorageMp string
	DstStorageMp string
	Progress     float64
	Status       string
	Mu           sync.Mutex
}

func (t *MoveTask) GetName() string {
	return fmt.Sprintf("move [%s](%s) to [%s](%s)", t.SrcStorageMp, t.SrcPath, t.DstStorageMp, t.DstPath)
}

func (t *MoveTask) GetStatus() string {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	return t.Status
}

func (t *MoveTask) GetProgress() float64 {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	return t.Progress
}

func (t *MoveTask) Run() error {
	t.ReinitCtx()
	t.SetStartTime(time.Now())
	defer t.SetEndTime(time.Now())

	srcObjs, err := collectObjects(t.Ctx(), t.SrcStorage, t.SrcPath)
	if err != nil {
		return err
	}

	total := len(srcObjs)
	copied := 0
	for _, obj := range srcObjs {
		if err := copySingle(t, obj); err != nil {
			return err
		}
		copied++
		t.Mu.Lock()
		t.Progress = float64(copied) / float64(total)
		t.Mu.Unlock()
	}

	// 验证复制完整性
	dstObjs, err := collectObjects(t.Ctx(), t.DstStorage, t.DstPath)
	if err != nil {
		return err
	}

	if len(srcObjs) != len(dstObjs) {
		return fmt.Errorf("mismatch: src %d files, dst %d files", len(srcObjs), len(dstObjs))
	}

	// 全部复制成功后统一删除
	for _, obj := range srcObjs {
		err := op.Delete(t.Ctx(), t.SrcStorage, obj.GetPath())
		if err != nil {
			return err
		}
	}

	t.Mu.Lock()
	t.Status = "done"
	t.Mu.Unlock()
	return nil
}

var moveTaskManager = tache.NewManager[*MoveTask]()

func Move(ctx context.Context, srcPath, dstPath string) (task.TaskExtensionInfo, error) {
	return _move(ctx, srcPath, dstPath)
}

func _move(ctx context.Context, srcPath, dstPath string) (task.TaskExtensionInfo, error) {
	srcStorage, srcActualPath, err := op.GetStorageAndActualPath(srcPath)
	if err != nil {
		return nil, err
	}
	dstStorage, dstActualPath, err := op.GetStorageAndActualPath(dstPath)
	if err != nil {
		return nil, err
	}

	if srcStorage.GetStorage() == dstStorage.GetStorage() {
		return nil, op.Move(ctx, srcStorage, srcActualPath, dstActualPath)
	}

	if ctx.Value(conf.NoTaskKey) != nil {
		return nil, errors.New("NoTaskKey not supported for cross-storage move")
	}

	taskCreator, _ := ctx.Value("user").(*model.User)
	t := &MoveTask{
		TaskExtension: task.TaskExtension{Creator: taskCreator},
		SrcStorage:    srcStorage,
		DstStorage:    dstStorage,
		SrcPath:       srcActualPath,
		DstPath:       dstActualPath,
		SrcStorageMp:  srcStorage.GetStorage().MountPath,
		DstStorageMp:  dstStorage.GetStorage().MountPath,
		Status:        "init",
	}
	moveTaskManager.Add(t)
	return t, nil
}

func collectObjects(ctx context.Context, storage driver.Driver, root string) ([]model.Obj, error) {
	var result []model.Obj
	obj, err := op.Get(ctx, storage, root)
	if err != nil {
		return nil, err
	}
	if obj.GetType() != model.TypeDir {
		result = append(result, obj)
		return result, nil
	}
	objs, err := op.List(ctx, storage, root, model.ListArgs{})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		subPath := path.Join(root, o.GetName())
		subObjs, err := collectObjects(ctx, storage, subPath)
		if err != nil {
			return nil, err
		}
		result = append(result, subObjs...)
	}
	return result, nil
}

func copySingle(t *MoveTask, obj model.Obj) error {
	src := obj.GetPath()
	rel := strings.TrimPrefix(src, t.SrcPath)
	dst := path.Join(t.DstPath, rel)
	link, _, err := op.Link(t.Ctx(), t.SrcStorage, src, model.LinkArgs{Header: http.Header{}})
	if err != nil {
		return err
	}
	fs := stream.FileStream{Obj: obj, Ctx: t.Ctx()}
	ss, err := stream.NewSeekableStream(fs, link)
	if err != nil {
		return err
	}
	return op.Put(t.Ctx(), t.DstStorage, dst, ss, nil, true)
}

// GetMoveProgress 返回指定任务的状态与进度（给前端用）
func GetMoveProgress(id string) (float64, string, error) {
	t := moveTaskManager.Get(id)
	if t == nil {
		return 0, "not found", fmt.Errorf("task not found")
	}
	return t.GetProgress(), t.GetStatus(), nil
}
