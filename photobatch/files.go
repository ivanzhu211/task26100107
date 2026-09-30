package photobatch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 本文件负责照片文件的原子落盘与清理。
// 约定：最终文件 <dir>/photos/<keyHash>/<n>.orig / <n>.thumb，
// 暂存文件以 .tmp 结尾，恢复时统一清除。

const photosDirName = "photos"

// batchDir 返回批次目录名。对幂等键取哈希，避免路径穿越与非法字符。
func batchDir(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func (s *Store) batchPath(key string) string {
	return filepath.Join(s.dir, photosDirName, batchDir(key))
}

func origPath(dir string, no int) string  { return filepath.Join(dir, fmt.Sprintf("%d.orig", no)) }
func thumbPath(dir string, no int) string { return filepath.Join(dir, fmt.Sprintf("%d.thumb", no)) }

// writeFileAtomic 将 data 原子写入 path：先写同目录 .tmp 并 fsync，再 rename，最后 fsync 目录。
// 崩溃后只会留下完整文件或 .tmp 暂存文件，不会出现半文件。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// removeTmpFiles 删除 dir 下所有 .tmp 暂存文件（不递归子目录之外的层级）。
func removeTmpFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// removeBatchFiles 删除批次目录及其全部图片文件。
func removeBatchFiles(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}
