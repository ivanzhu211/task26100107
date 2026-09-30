package photobatch

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// ttl 未提交批次的有效期限。
const ttl = 24 * time.Hour

const (
	minTotal = 1
	maxTotal = 50
)

type batchRow struct {
	key         string
	keyHash     string
	site        string
	stage       string
	total       int
	firstRecvAt time.Time
	submittedAt *time.Time
}

func (b *batchRow) expired(now time.Time) bool {
	return b.submittedAt == nil && !now.Before(b.firstRecvAt.Add(ttl))
}

func (s *Store) loadBatch(key string) (*batchRow, error) {
	var (
		b       batchRow
		firstNs int64
		subNs   sql.NullInt64
	)
	err := s.db.QueryRow(`SELECT key,key_hash,site,stage,total,first_recv_at,submitted_at
		FROM batches WHERE key=?`, key).
		Scan(&b.key, &b.keyHash, &b.site, &b.stage, &b.total, &firstNs, &subNs)
	if err != nil {
		return nil, err
	}
	b.firstRecvAt = time.Unix(0, firstNs).UTC()
	if subNs.Valid {
		t := time.Unix(0, subNs.Int64).UTC()
		b.submittedAt = &t
	}
	return &b, nil
}

// Receive 接收编号 no 的照片对。now 为调用方提供的 UTC 业务时间，需单调不减。
// 相同内容重复发送视为同一次接收；内容不同返回 ErrPhotoConflict。
func (s *Store) Receive(now time.Time, key, site, stage string, total, no int, original, thumbnail []byte) error {
	if key == "" || site == "" || stage == "" {
		return fmt.Errorf("%w: key/site/stage must be non-empty", ErrInvalidArgument)
	}
	if total < minTotal || total > maxTotal {
		return fmt.Errorf("%w: total must be in [1,50]", ErrInvalidArgument)
	}
	if no < 1 || no > total {
		return fmt.Errorf("%w: photo number out of range", ErrInvalidArgument)
	}
	if len(original) == 0 || len(thumbnail) == 0 {
		return fmt.Errorf("%w: empty photo content", ErrInvalidArgument)
	}
	if err := s.checkClock(nil, now); err != nil {
		return err
	}

	b, err := s.loadBatch(key)
	if errors.Is(err, sql.ErrNoRows) {
		b, err = s.createBatch(now, key, site, stage, total)
	}
	if err != nil {
		return err
	}
	if b.site != site || b.stage != stage || b.total != total {
		return ErrKeyConflict
	}
	if b.submittedAt != nil {
		return ErrAlreadySubmitted
	}
	if b.expired(now) {
		return ErrExpired
	}

	origSha := sha256.Sum256(original)
	thumbSha := sha256.Sum256(thumbnail)

	var dbOrig, dbThumb []byte
	err = s.db.QueryRow(`SELECT orig_sha,thumb_sha FROM photos WHERE key=? AND no=?`,
		key, no).Scan(&dbOrig, &dbThumb)
	switch {
	case err == nil:
		if bytes.Equal(dbOrig, origSha[:]) && bytes.Equal(dbThumb, thumbSha[:]) {
			return nil // 同一次接收的重试
		}
		return ErrPhotoConflict
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	// 无接收记录：落盘照片对，再登记。落盘与登记之间的崩溃残留
	// 未被确认，允许被后续接收重写，不产生半张已接收照片。
	dir := s.batchPath(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(origPath(dir, no), original); err != nil {
		return err
	}
	s.hit("receive.orig-written")
	if err := writeFileAtomic(thumbPath(dir, no), thumbnail); err != nil {
		return err
	}
	s.hit("receive.pair-written")

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO photos(key,no,orig_sha,thumb_sha) VALUES(?,?,?,?)`,
		key, no, origSha[:], thumbSha[:]); err != nil {
		return err
	}
	if err := s.checkClock(tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.hit("receive.committed")
	return nil
}

func (s *Store) createBatch(now time.Time, key, site, stage string, total int) (*batchRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO batches(key,key_hash,site,stage,total,first_recv_at)
		VALUES(?,?,?,?,?,?)`,
		key, batchDir(key), site, stage, total, now.UnixNano()); err != nil {
		return nil, err
	}
	if err := s.checkClock(tx, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.hit("receive.batch-created")
	return s.loadBatch(key)
}

// Submit 提交批次。全部编号收齐后才允许提交；重复提交返回原有结果。
func (s *Store) Submit(now time.Time, key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidArgument)
	}
	if err := s.checkClock(nil, now); err != nil {
		return err
	}
	b, err := s.loadBatch(key)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if b.submittedAt != nil {
		return nil // 重复提交
	}
	if b.expired(now) {
		return ErrExpired
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM photos WHERE key=?`, key).Scan(&n); err != nil {
		return err
	}
	if n != b.total {
		return ErrIncomplete
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE batches SET submitted_at=? WHERE key=? AND submitted_at IS NULL`,
		now.UnixNano(), key); err != nil {
		return err
	}
	if err := s.checkClock(tx, now); err != nil {
		return err
	}
	s.hit("submit.before-commit")
	if err := tx.Commit(); err != nil {
		return err
	}
	s.hit("submit.committed")
	return nil
}

// Query 查询批次状态与已接收编号，不返回照片内容。
func (s *Store) Query(now time.Time, key string) (*BatchInfo, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: empty key", ErrInvalidArgument)
	}
	if err := s.checkClock(nil, now); err != nil {
		return nil, err
	}
	b, err := s.loadBatch(key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT no FROM photos WHERE key=? ORDER BY no`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var received []int
	for rows.Next() {
		var no int
		if err := rows.Scan(&no); err != nil {
			return nil, err
		}
		received = append(received, no)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	info := &BatchInfo{
		Key:       b.key,
		Site:      b.site,
		Stage:     b.stage,
		Total:     b.total,
		Received:  received,
		FirstRecv: b.firstRecvAt,
		Submitted: b.submittedAt != nil,
	}
	switch {
	case b.submittedAt != nil:
		info.Status = StatusSubmitted
	case b.expired(now):
		info.Status = StatusExpired
	case len(received) == b.total:
		info.Status = StatusReady
	default:
		info.Status = StatusReceiving
	}
	return info, nil
}

// ReadPhotos 读取已提交批次的整批照片（编号 1..total 顺序）。
func (s *Store) ReadPhotos(key string) ([]Photo, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: empty key", ErrInvalidArgument)
	}
	b, err := s.loadBatch(key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if b.submittedAt == nil {
		return nil, ErrNotSubmitted
	}
	dir := s.batchPath(key)
	photos := make([]Photo, 0, b.total)
	for no := 1; no <= b.total; no++ {
		orig, err := os.ReadFile(origPath(dir, no))
		if err != nil {
			return nil, fmt.Errorf("read original %d: %w", no, err)
		}
		thumb, err := os.ReadFile(thumbPath(dir, no))
		if err != nil {
			return nil, fmt.Errorf("read thumbnail %d: %w", no, err)
		}
		photos = append(photos, Photo{Original: orig, Thumbnail: thumb})
	}
	return photos, nil
}

// Cleanup 清理过期批次与已放弃接收留下的图片文件。
// 返回后，应释放的图片文件必须全部消失；过期键的识别状态保留。
func (s *Store) Cleanup(now time.Time) error {
	if err := s.checkClock(nil, now); err != nil {
		return err
	}
	// 先执行与 Open 相同的幂等恢复，回收 .tmp 与孤儿目录。
	if err := s.recover(); err != nil {
		return err
	}
	s.hit("cleanup.recovered")

	rows, err := s.db.Query(`SELECT key,key_hash,site,stage,total,first_recv_at,submitted_at
		FROM batches WHERE submitted_at IS NULL`)
	if err != nil {
		return err
	}
	var expired []*batchRow
	for rows.Next() {
		var (
			b       batchRow
			firstNs int64
			subNs   sql.NullInt64
		)
		if err := rows.Scan(&b.key, &b.keyHash, &b.site, &b.stage, &b.total, &firstNs, &subNs); err != nil {
			rows.Close()
			return err
		}
		b.firstRecvAt = time.Unix(0, firstNs).UTC()
		if b.expired(now) {
			expired = append(expired, &b)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	sort.Slice(expired, func(i, j int) bool { return expired[i].key < expired[j].key })

	for _, b := range expired {
		// 先删文件再删照片记录；任一点崩溃都可由下次 Cleanup 幂等续做。
		if err := removeBatchFiles(s.batchPath(b.key)); err != nil {
			return err
		}
		s.hit("cleanup.files-removed")
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM photos WHERE key=?`, b.key); err != nil {
			tx.Rollback()
			return err
		}
		if err := s.checkClock(tx, now); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.hit("cleanup.rows-removed")
	}
	return nil
}
