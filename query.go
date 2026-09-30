package photobatch

import (
	"context"
	"time"
)

// Query 查询批次状态与已接收编号，不提供照片内容。
func (s *Store) Query(ctx context.Context, key string, now time.Time) (BatchInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchInfo{}, err
	}
	defer tx.Rollback()

	effNow, err := effectiveNow(tx, now)
	if err != nil {
		return BatchInfo{}, err
	}
	b, err := getBatch(tx, key)
	if err != nil {
		return BatchInfo{}, err
	}
	if b == nil {
		return BatchInfo{}, ErrBatchNotFound
	}
	rows, err := tx.Query(`SELECT seq FROM photos WHERE batch_key = ? ORDER BY seq`, key)
	if err != nil {
		return BatchInfo{}, err
	}
	var seqs []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return BatchInfo{}, err
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return BatchInfo{}, err
	}
	rows.Close()

	info := BatchInfo{
		Key:            b.key,
		Site:           b.site,
		Stage:          b.stage,
		Total:          b.total,
		Status:         b.status(effNow, len(seqs)),
		ReceivedSeqs:   seqs,
		FirstReceiveAt: b.firstReceiveAt,
		ExpiresAt:      b.expiresAt(),
	}
	if b.submittedAt != nil {
		info.SubmittedAt = *b.submittedAt
	}
	return info, tx.Commit()
}
