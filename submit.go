package photobatch

import (
	"context"
	"os"
	"time"
)

// Submit 提交批次。全部编号接收成功后才允许提交；
// 提交是单个 SQLite 事务，要么整批公开、要么不生效。
// 重复提交返回原有结果。
func (s *Store) Submit(ctx context.Context, key string, now time.Time) (SubmitResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SubmitResult{}, err
	}
	defer tx.Rollback()

	effNow, err := effectiveNow(tx, now)
	if err != nil {
		return SubmitResult{}, err
	}
	b, err := getBatch(tx, key)
	if err != nil {
		return SubmitResult{}, err
	}
	if b == nil {
		return SubmitResult{}, ErrBatchNotFound
	}
	if b.submittedAt != nil {
		res := SubmitResult{Key: b.key, Total: b.total, SubmittedAt: *b.submittedAt}
		return res, tx.Commit()
	}
	if b.expired || b.isExpired(effNow) {
		if _, err := tx.Exec(`UPDATE batches SET expired = 1 WHERE key = ?`, key); err != nil {
			return SubmitResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return SubmitResult{}, err
		}
		return SubmitResult{}, ErrBatchExpired
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM photos WHERE batch_key = ?`, key).Scan(&count); err != nil {
		return SubmitResult{}, err
	}
	if count < b.total {
		return SubmitResult{}, ErrBatchIncomplete
	}
	if _, err := tx.Exec(`UPDATE batches SET submitted_at = ? WHERE key = ?`,
		effNow.UnixNano(), key); err != nil {
		return SubmitResult{}, err
	}
	crashPoint("submit_before_commit")
	if err := tx.Commit(); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Key: b.key, Total: b.total, SubmittedAt: effNow}, nil
}

// ReadPhoto 读取已提交批次中某张照片的原图与缩略图。
func (s *Store) ReadPhoto(ctx context.Context, key string, seq int) (original, thumbnail []byte, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	b, err := getBatch(tx, key)
	if err != nil {
		return nil, nil, err
	}
	if b == nil {
		return nil, nil, ErrBatchNotFound
	}
	if b.submittedAt == nil {
		return nil, nil, ErrNotSubmitted
	}
	if seq < 1 || seq > b.total {
		return nil, nil, ErrInvalidInput
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	original, err = os.ReadFile(photoPath(s.photosDir(), key, seq, "orig"))
	if err != nil {
		return nil, nil, err
	}
	thumbnail, err = os.ReadFile(photoPath(s.photosDir(), key, seq, "thumb"))
	if err != nil {
		return nil, nil, err
	}
	return original, thumbnail, nil
}
