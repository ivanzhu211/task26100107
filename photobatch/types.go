package photobatch

import "time"

// Status 批次状态。
type Status int

const (
	StatusReceiving Status = iota // 接收中，尚未收齐
	StatusReady                   // 已收齐，未提交
	StatusSubmitted               // 已提交，公开可读
	StatusExpired                 // 未提交且已超过期限
)

func (s Status) String() string {
	switch s {
	case StatusReceiving:
		return "receiving"
	case StatusReady:
		return "ready"
	case StatusSubmitted:
		return "submitted"
	case StatusExpired:
		return "expired"
	}
	return "unknown"
}

// BatchInfo 批次查询结果，不含照片内容。
type BatchInfo struct {
	Key       string
	Site      string
	Stage     string
	Total     int
	Received  []int // 已完整接收的照片编号，升序
	Status    Status
	FirstRecv time.Time
	Submitted bool
}

// Photo 一张照片对（原图 + 300px 缩略图）。
type Photo struct {
	Original  []byte
	Thumbnail []byte
}
