package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model/tables"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func CreateSliceUpload(su *tables.SliceUpload) error {
	return errors.WithStack(db.Create(su).Error)
}

func GetSliceUpload(wh map[string]any) (*tables.SliceUpload, error) {
	su := &tables.SliceUpload{}
	return su, db.Where(wh).First(su).Error
}

// GetSliceUploadByTaskID 通过TaskID获取分片上传记录
func GetSliceUploadByTaskID(taskID string) (*tables.SliceUpload, error) {
	su := &tables.SliceUpload{}
	return su, db.Where("task_id = ?", taskID).First(su).Error
}

func UpdateSliceUpload(su *tables.SliceUpload) error {
	return errors.WithStack(db.Save(su).Error)
}

// DeleteSliceUpload 删除分片上传记录
func DeleteSliceUpload(id uint) error {
	return errors.WithStack(db.Delete(&tables.SliceUpload{}, id).Error)
}

// DeleteSliceUploadByTaskID 通过TaskID删除分片上传记录
func DeleteSliceUploadByTaskID(taskID string) error {
	return errors.WithStack(db.Where("task_id = ?", taskID).Delete(&tables.SliceUpload{}).Error)
}

// UpdateSliceUploadWithTx 使用事务更新分片上传状态，确保数据一致性
func UpdateSliceUploadWithTx(su *tables.SliceUpload) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		return tx.Save(su).Error
	}))
}

// UpdateSliceStatusAtomic 原子性地更新分片状态
func UpdateSliceStatusAtomic(taskID string, sliceNum int, status []byte) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		// 先读取当前状态
		var su tables.SliceUpload
		if err := tx.Where("task_id = ?", taskID).First(&su).Error; err != nil {
			return err
		}
		
		// 更新分片状态
		tables.SetSliceUploaded(su.SliceUploadStatus, sliceNum)
		
		// 保存更新
		return tx.Save(&su).Error
	}))
}
