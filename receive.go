package photobatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strings"
	"time"
)

// validKey 校验幂等键可安全用作目录名。
func validKey(key string) bool {
	if key == "" || key == "." || key == ".." {
		return false
	}
	return !strings.ContainsAny(key, "/\\\x00")
}

// photoHashes 是一条已接收照片记录的内容摘要。
type photoHashes struct {
	origHash  []byte
	thumbHash []byte
}

func getPhoto(tx *sql.Tx, key string, seq int) (*photoHashes, error) {
	var p photoHashes
	err := tx.QueryRow(`SELECT orig_hash, thumb_hash FROM photos WHERE batch_key = ? AND seq = ?`,
		key, seq).Scan(&p.origHash, &p.thumbHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func equalHash(stored, sum []byte) bool {
	if len(stored) != len(sum) {
		return false
	}
	for i := range stored {
		if stored[i] != sum[i] {
			return false
		}
	}
	return true
}

// ReceivePhoto 接收批次中的一张照片，原图与缩略图都完整落盘并登记后才算成功。
// 同一键首次使用时固定工地、阶段与总数；再次使用必须一致。
// 同一编号重复发送完全相同的内容视为同一次接收；内容不同则拒绝。
func (s *Store) ReceivePhoto(ctx context.Context, key, site, stage string, total, seq int, original, thumbnail []byte, now time.Time) error {
	if !validKey(key) || site == "" || stage == "" ||
		total < MinTotal || total > MaxTotal ||
		seq < 1 || seq > total ||
		len(original) == 0 || len(thumbnail) == 0 {
		return ErrInvalidInput
	}
	origSum := sha256.Sum256(original)
	thumbSum := sha256.Sum256(thumbnail)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	effNow, err := effectiveNow(tx, now)
	if err != nil {
		return err
	}
	b, err := getBatch(tx, key)
	if err != nil {
		return err
	}

	if b != nil {
		if b.site != site || b.stage != stage || b.total != total {
			return ErrBatchMismatch
		}
		if b.submittedAt != nil {
			// 已提交批次不可修改；完全相同的重放视为同一次接收。
			existing, err := getPhoto(tx, key, seq)
			if err != nil {
				return err
			}
			if existing != nil &&
				equalHash(existing.origHash, origSum[:]) &&
				equalHash(existing.thumbHash, thumbSum[:]) {
				return tx.Commit()
			}
			return ErrPhotoConflict
		}
		if b.expired || b.isExpired(effNow) {
			if _, err := tx.Exec(`UPDATE batches SET expired = 1 WHERE key = ?`, key); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return ErrBatchExpired
		}
		existing, err := getPhoto(tx, key, seq)
		if err != nil {
			return err
		}
		if existing != nil {
			if equalHash(existing.origHash, origSum[:]) &&
				equalHash(existing.thumbHash, thumbSum[:]) {
				return tx.Commit()
			}
			return ErrPhotoConflict
		}
	}

	// 先写图片文件（原子 rename），再在事务内登记。
	// 崩溃只会留下孤儿文件，由 Cleanup 回收，不会产生半张已登记照片。
	if err := s.writeFileAtomic(photoPath(s.photosDir(), key, seq, "orig"), original); err != nil {
		return err
	}
	crashPoint("receive_after_orig")
	if err := s.writeFileAtomic(photoPath(s.photosDir(), key, seq, "thumb"), thumbnail); err != nil {
		return err
	}
	crashPoint("receive_after_files")

	if b == nil {
		if _, err := tx.Exec(`INSERT INTO batches
			(key, site, stage, total, first_receive_at, submitted_at, expired)
			VALUES (?, ?, ?, ?, ?, NULL, 0)`,
			key, site, stage, total, effNow.UnixNano()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO photos (batch_key, seq, orig_hash, thumb_hash)
		VALUES (?, ?, ?, ?)`, key, seq, origSum[:], thumbSum[:]); err != nil {
		return err
	}
	crashPoint("receive_before_commit")
	return tx.Commit()
}
