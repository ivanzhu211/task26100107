package photobatch

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store 是批次入库组件，仅供单进程串行调用。
type Store struct {
	db  *sql.DB
	dir string
}

const schema = `
CREATE TABLE IF NOT EXISTS batches (
  key              TEXT PRIMARY KEY,
  site             TEXT NOT NULL,
  stage            TEXT NOT NULL,
  total            INTEGER NOT NULL,
  first_receive_at INTEGER NOT NULL,
  submitted_at     INTEGER,
  expired          INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS photos (
  batch_key  TEXT NOT NULL REFERENCES batches(key),
  seq        INTEGER NOT NULL,
  orig_hash  BLOB NOT NULL,
  thumb_hash BLOB NOT NULL,
  PRIMARY KEY (batch_key, seq)
);
CREATE TABLE IF NOT EXISTS meta (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  last_now INTEGER NOT NULL
);
`

// Open 打开（必要时创建）位于 dir 的存储。
// 所有状态迁移都在 SQLite 事务内完成，图片先落盘再登记，
// 因此进程在任意时刻被杀后重新 Open 都能得到一致状态；
// 崩溃遗留的孤儿文件由 Cleanup 回收。
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, ErrInvalidInput
	}
	for _, d := range []string{dir, filepath.Join(dir, "photos"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "batches.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO meta (id, last_now) VALUES (1, 0)
		ON CONFLICT(id) DO NOTHING`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
}

// Close 关闭存储。
func (s *Store) Close() error {
	return s.db.Close()
}

// effectiveNow 在事务内把调用方提供的业务时间钳制为单调不减，
// 并持久化已见的最大时刻，保证重启后期限判定不回退。
func effectiveNow(tx *sql.Tx, now time.Time) (time.Time, error) {
	now = now.UTC()
	var last int64
	if err := tx.QueryRow(`SELECT last_now FROM meta WHERE id = 1`).Scan(&last); err != nil {
		return time.Time{}, err
	}
	nano := now.UnixNano()
	if nano < last {
		nano = last
	} else if nano > last {
		if _, err := tx.Exec(`UPDATE meta SET last_now = ? WHERE id = 1`, nano); err != nil {
			return time.Time{}, err
		}
	}
	return time.Unix(0, nano).UTC(), nil
}

// batchRow 是 batches 表的一行。
type batchRow struct {
	key            string
	site           string
	stage          string
	total          int
	firstReceiveAt time.Time
	submittedAt    *time.Time
	expired        bool
}

func scanBatch(row interface{ Scan(...any) error }) (batchRow, error) {
	var b batchRow
	var firstNano int64
	var subNano sql.NullInt64
	var expired int
	err := row.Scan(&b.key, &b.site, &b.stage, &b.total, &firstNano, &subNano, &expired)
	if err != nil {
		return b, err
	}
	b.firstReceiveAt = time.Unix(0, firstNano).UTC()
	if subNano.Valid {
		t := time.Unix(0, subNano.Int64).UTC()
		b.submittedAt = &t
	}
	b.expired = expired != 0
	return b, nil
}

const batchCols = `key, site, stage, total, first_receive_at, submitted_at, expired`

// getBatch 在事务内按键取批次，不存在返回 nil。
func getBatch(tx *sql.Tx, key string) (*batchRow, error) {
	row := tx.QueryRow(`SELECT `+batchCols+` FROM batches WHERE key = ?`, key)
	b, err := scanBatch(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// expiresAt 返回未提交批次的过期时刻。
func (b *batchRow) expiresAt() time.Time { return b.firstReceiveAt.Add(TTL) }

// isExpired 判断批次在 now 时刻是否已过期（未提交且到达期限）。
func (b *batchRow) isExpired(now time.Time) bool {
	return b.submittedAt == nil && !now.Before(b.expiresAt())
}

// status 计算批次在 now 时刻的状态。
func (b *batchRow) status(now time.Time, receivedCount int) Status {
	switch {
	case b.submittedAt != nil:
		return StatusSubmitted
	case b.expired || b.isExpired(now):
		return StatusExpired
	case receivedCount >= b.total:
		return StatusReady
	default:
		return StatusReceiving
	}
}
