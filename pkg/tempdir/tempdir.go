package tempdir

import (
	"os"
	"path/filepath"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

// GetPersistentTempDir 获取持久化临时目录
// 这个函数被多个包共享使用，避免代码重复
func GetPersistentTempDir() string {
	var tempDir string
	
	// 优先使用配置的临时目录
	if conf.Conf != nil && conf.Conf.TempDir != "" {
		tempDir = conf.Conf.TempDir
	} else {
		// 使用数据目录下的slice_temp子目录
		if conf.Conf != nil && conf.Conf.Database.DBFile != "" {
			// 从数据库文件路径推断数据目录
			dataDir := filepath.Dir(conf.Conf.Database.DBFile)
			tempDir = filepath.Join(dataDir, "slice_temp")
		} else {
			// fallback到当前工作目录下的slice_temp
			if wd, err := os.Getwd(); err == nil {
				tempDir = filepath.Join(wd, "slice_temp")
			} else {
				// 最后的fallback
				tempDir = filepath.Join(os.TempDir(), "openlist_slice_temp")
			}
		}
	}
	
	return tempDir
}
