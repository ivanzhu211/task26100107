package photobatch

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// Cleanup 清理过期批次与废弃的图片文件。
// 先在事务内把到期未提交的批次标记为过期并删除其照片登记，
// 再删除对应图片文件；崩溃后重跑幂等，最终无残留。
// 已提交批次与尚未过期的接收记录不受影响。
func (s *Store) Cleanup(ctx context.Context, now time.Time) (CleanupResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupResult{}, err
	}
	defer tx.Rollback()

	effNow, err := effectiveNow(tx, now)
	if err != nil {
		return CleanupResult{}, err
	}
	// 选出所有已确认过期或已到期未提交的批次。
	rows, err := tx.Query(`SELECT key, first_receive_at, expired FROM batches WHERE submitted_at IS NULL`)
	if err != nil {
		return CleanupResult{}, err
	}
	type victim struct {
		key     string
		firstAt time.Time
		expired bool
	}
	var victims []victim
	for rows.Next() {
		var v victim
		var firstNano int64
		var expired int
		if err := rows.Scan(&v.key, &firstNano, &expired); err != nil {
			rows.Close()
			return CleanupResult{}, err
		}
		v.firstAt = time.Unix(0, firstNano).UTC()
		v.expired = expired != 0
		victims = append(victims, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CleanupResult{}, err
	}
	rows.Close()

	var res CleanupResult
	for _, v := range victims {
		if !v.expired && effNow.Before(v.firstAt.Add(TTL)) {
			continue // 尚未过期
		}
		if _, err := tx.Exec(`UPDATE batches SET expired = 1 WHERE key = ?`, v.key); err != nil {
			return CleanupResult{}, err
		}
		if _, err := tx.Exec(`DELETE FROM photos WHERE batch_key = ?`, v.key); err != nil {
			return CleanupResult{}, err
		}
		res.ExpiredKeys = append(res.ExpiredKeys, v.key)
	}
	if err := tx.Commit(); err != nil {
		return CleanupResult{}, err
	}
	crashPoint("cleanup_after_mark")

	// 事务外删除图片文件；进程被杀后下次清理会重试，RemoveAll 幂等。
	for i, key := range res.ExpiredKeys {
		if err := s.removeBatchFiles(key); err != nil {
			return res, err
		}
		if i == 0 {
			crashPoint("cleanup_after_first_delete")
		}
	}

	// 回收孤儿文件：已登记且属于有效批次的照片保留，其余删除。
	keep, err := s.livePhotoSet(ctx)
	if err != nil {
		return res, err
	}
	if err := s.sweepOrphans(func(key string, seq int, kind string) bool {
		_, ok := keep[photoRef{key: key, seq: seq}]
		return ok
	}); err != nil {
		return res, err
	}

	// 清理写入中断留下的临时文件。
	if tmpEntries, err := os.ReadDir(s.tmpDir()); err == nil {
		for _, e := range tmpEntries {
			if err := os.RemoveAll(filepath.Join(s.tmpDir(), e.Name())); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

type photoRef struct {
	key string
	seq int
}

// livePhotoSet 返回当前所有已登记照片的集合。
func (s *Store) livePhotoSet(ctx context.Context) (map[photoRef]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT batch_key, seq FROM photos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := make(map[photoRef]struct{})
	for rows.Next() {
		var r photoRef
		if err := rows.Scan(&r.key, &r.seq); err != nil {
			return nil, err
		}
		set[r] = struct{}{}
	}
	return set, rows.Err()
}
