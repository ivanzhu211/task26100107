// Package photobatch 提供屋面现场照片的批次入库能力。
// 批次状态存 SQLite，原图与缩略图存本地目录。
package photobatch

import (
	"errors"
	"time"
)

// 批次照片数量限制。
const (
	MinTotal = 1
	MaxTotal = 50
)

// TTL 是未提交批次从首次成功接收照片起的有效期限。
const TTL = 24 * time.Hour

// Status 表示批次当前状态。
type Status string

const (
	StatusReceiving Status = "receiving" // 已创建，尚未收齐
	StatusReady     Status = "ready"     // 已收齐，未提交
	StatusSubmitted Status = "submitted" // 已提交，内容公开
	StatusExpired   Status = "expired"   // 未提交且已过期限
)

// 组件返回的错误。
var (
	ErrInvalidInput    = errors.New("photobatch: invalid input")
	ErrBatchNotFound   = errors.New("photobatch: batch not found")
	ErrBatchMismatch   = errors.New("photobatch: idempotency key reused with different batch identity")
	ErrBatchExpired    = errors.New("photobatch: batch expired")
	ErrPhotoConflict   = errors.New("photobatch: photo content conflicts with previously received data")
	ErrBatchIncomplete = errors.New("photobatch: batch is not fully received")
	ErrNotSubmitted    = errors.New("photobatch: batch is not submitted")
)

// BatchInfo 是批次查询结果。待提交批次仅包含状态与已接收编号。
type BatchInfo struct {
	Key            string
	Site           string
	Stage          string
	Total          int
	Status         Status
	ReceivedSeqs   []int // 已接收成功的照片编号（升序）
	FirstReceiveAt time.Time
	SubmittedAt    time.Time // 零值表示未提交
	ExpiresAt      time.Time // 未提交批次的过期时刻
}

// SubmitResult 是提交批次的结果。
type SubmitResult struct {
	Key         string
	Total       int
	SubmittedAt time.Time
}

// CleanupResult 是清理过期批次的结果。
type CleanupResult struct {
	ExpiredKeys []string // 本次清理中确认过期并释放图片的批次键
}
