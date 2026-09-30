package photobatch

import "errors"

var (
	// ErrKeyConflict 幂等键已存在但工地/阶段/总数不一致。
	ErrKeyConflict = errors.New("photobatch: idempotency key conflict")
	// ErrPhotoConflict 同一编号照片内容与此前的接收记录不一致。
	ErrPhotoConflict = errors.New("photobatch: photo content conflict")
	// ErrExpired 批次已过期，不能再接收或提交。
	ErrExpired = errors.New("photobatch: batch expired")
	// ErrIncomplete 批次尚未收齐，不能提交。
	ErrIncomplete = errors.New("photobatch: batch incomplete")
	// ErrAlreadySubmitted 批次已提交，不可再修改照片。
	ErrAlreadySubmitted = errors.New("photobatch: batch already submitted")
	// ErrNotFound 批次不存在。
	ErrNotFound = errors.New("photobatch: batch not found")
	// ErrNotSubmitted 批次未提交，不能读取照片内容。
	ErrNotSubmitted = errors.New("photobatch: batch not submitted")
	// ErrInvalidArgument 参数非法。
	ErrInvalidArgument = errors.New("photobatch: invalid argument")
	// ErrNonMonotonicClock 调用方提供的时间发生回退。
	ErrNonMonotonicClock = errors.New("photobatch: non-monotonic clock")
)
