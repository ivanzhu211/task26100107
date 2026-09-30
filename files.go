package photobatch

import (
	"fmt"
	"os"
	"path/filepath"
)

// 目录布局：
//   dir/batches.db          SQLite 状态库
//   dir/photos/<key>/<seq>.orig  原图
//   dir/photos/<key>/<seq>.thumb 缩略图
//   dir/tmp/                写入中的临时文件（rename 前）

func (s *Store) photosDir() string { return filepath.Join(s.dir, "photos") }
func (s *Store) tmpDir() string    { return filepath.Join(s.dir, "tmp") }

func (s *Store) batchDir(key string) string {
	return filepath.Join(s.photosDir(), key)
}

func photoPath(dir, key string, seq int, kind string) string {
	return filepath.Join(dir, key, fmt.Sprintf("%d.%s", seq, kind))
}

// writeFileAtomic 将 data 原子写入 path：先写临时文件并 fsync，再 rename。
// 进程在任意时刻被杀，path 处要么是旧内容、要么是完整新内容。
func (s *Store) writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.tmpDir(), "w-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// removeBatchFiles 删除一个批次的全部图片文件及其目录。幂等。
func (s *Store) removeBatchFiles(key string) error {
	err := os.RemoveAll(s.batchDir(key))
	if err != nil {
		return err
	}
	return nil
}

// sweepOrphans 删除 photos 目录下未被 keep(key,seq) 认可的文件及空目录。
// keep 返回 true 表示该文件对应一条有效的已接收记录，必须保留。
func (s *Store) sweepOrphans(keep func(key string, seq int, kind string) bool) error {
	root := s.photosDir()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			// 非目录残留直接删除
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				return err
			}
			continue
		}
		key := e.Name()
		bdir := filepath.Join(root, key)
		files, err := os.ReadDir(bdir)
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.IsDir() {
				if err := os.RemoveAll(filepath.Join(bdir, f.Name())); err != nil {
					return err
				}
				continue
			}
			var seq int
			var kind string
			if _, err := fmt.Sscanf(f.Name(), "%d.%s", &seq, &kind); err != nil {
				kind = ""
			}
			if (kind != "orig" && kind != "thumb") || !keep(key, seq, kind) {
				if err := os.Remove(filepath.Join(bdir, f.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
		// 空目录删除
		if rest, _ := os.ReadDir(bdir); len(rest) == 0 {
			if err := os.Remove(bdir); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
