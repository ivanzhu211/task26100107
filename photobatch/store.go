package photobatch

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store 批次入库组件。单进程串行使用，非并发安全。
type Store struct {
	db     *sql.DB
	dir    string
	last   time.Time // 已见的最大业务时间（UTC）
	crash  func(point string)
	closed bool
}

// Open 打开（必要时创建）位于 dir 的存储，并执行崩溃恢复。
// 恢复可重入：恢复过程中断不会破坏后续恢复。
func Open(dir string) (*Store, error) {
	return open(dir, nil)
}

// open 允许测试在恢复前注入崩溃钩子。
func open(dir string, crash func(string)) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: empty dir", ErrInvalidArgument)
	}
	if err := os.MkdirAll(filepath.Join(dir, photosDirName), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)",
		filepath.Join(dir, "state.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir}
	s.crash = crash
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.recover(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS batches(
			key TEXT PRIMARY KEY,
			key_hash TEXT NOT NULL,
			site TEXT NOT NULL,
			stage TEXT NOT NULL,
			total INTEGER NOT NULL,
			first_recv_at INTEGER NOT NULL,
			submitted_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS photos(
			key TEXT NOT NULL,
			no INTEGER NOT NULL,
			orig_sha BLOB NOT NULL,
			thumb_sha BLOB NOT NULL,
			PRIMARY KEY(key, no)
		)`,
		`CREATE TABLE IF NOT EXISTS meta(
			k TEXT PRIMARY KEY,
			v INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	var nanos int64
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='last_now'`).Scan(&nanos)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.last = time.Unix(0, nanos).UTC()
	return nil
}

// recover 清理崩溃残留：.tmp 暂存文件、无批次记录的孤儿目录。
// 所有操作幂等，可反复执行。
func (s *Store) recover() error {
	photosRoot := filepath.Join(s.dir, photosDirName)
	entries, err := os.ReadDir(photosRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s.hit("recover.dir")
		dir := filepath.Join(photosRoot, e.Name())
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM batches WHERE key_hash=?`, e.Name()).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if err := removeBatchFiles(dir); err != nil {
				return err
			}
			continue
		}
		if err := removeTmpFiles(dir); err != nil {
			return err
		}
	}
	return syncDir(photosRoot)
}

// Close 关闭存储。
func (s *Store) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// hit 测试注入点：模拟进程在关键位置被直接终止。
func (s *Store) hit(point string) {
	if s.crash != nil {
		s.crash(point)
	}
}

// checkClock 校验并推进单调业务时钟；持久化到 meta，跨重启生效。
// 调用方须已持有事务（若有）。
func (s *Store) checkClock(tx *sql.Tx, now time.Time) error {
	if now.Location() != time.UTC {
		return fmt.Errorf("%w: time must be UTC", ErrInvalidArgument)
	}
	if now.Before(s.last) {
		return ErrNonMonotonicClock
	}
	s.last = now
	if tx != nil {
		_, err := tx.Exec(`INSERT INTO meta(k,v) VALUES('last_now',?)
			ON CONFLICT(k) DO UPDATE SET v=excluded.v`, now.UnixNano())
		return err
	}
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES('last_now',?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, now.UnixNano())
	return err
}
