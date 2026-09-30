# photobatch

屋面现场照片批次入库组件（Go 库）。批次状态存 SQLite，原图与 300px 缩略图存本地目录。
仅供单进程串行调用；进程可在任意时刻被终止，重启后状态一致。

## 用法

```go
s, err := photobatch.Open("/var/lib/photos")   // 创建或恢复存储
defer s.Close()

now := time.Now().UTC() // 业务时间由调用方提供，UTC 且单调不减（组件内也会钳制）

// 接收：键首次使用时固定工地/阶段/总数（1~50），编号 1..total
err = s.ReceivePhoto(ctx, key, "工地A", "屋面施工", 30, 1, origBytes, thumbBytes, now)

// 查询：仅返回状态与已接收编号
info, err := s.Query(ctx, key, now)

// 提交：收齐后才允许；单事务整批公开；重复提交返回原结果
res, err := s.Submit(ctx, key, now)

// 提交后逐字节读回
orig, thumb, err := s.ReadPhoto(ctx, key, 1)

// 清理：释放过期批次与废弃图片文件，已提交批次不受影响
res, err := s.Cleanup(ctx, now)
```

## 规则要点

- 幂等键全局唯一；再次使用时工地/阶段/总数必须一致，否则 `ErrBatchMismatch`。
- 同一编号重复发送完全相同内容视为同一次接收；任一内容不同返回 `ErrPhotoConflict`，不覆盖。
- 一张照片原图+缩略图都完整落盘并登记后才算接收成功。
- 批次从首次成功接收起计时 24 小时；到期未提交即过期（`ErrBatchExpired`），即使清理未运行。
- 过期键永久不可复用；清理只删过期批次与孤儿文件，已提交批次不受影响。

## 崩溃一致性

- 图片先写临时文件并 fsync，再原子 rename，最后在 SQLite 事务内登记；
  崩溃只产生孤儿文件，由 `Cleanup` 回收，不会出现半张已登记照片。
- 提交是单个 SQLite 事务，要么整批公开要么不生效；重试结果一致。
- 清理先在事务内标记过期，再删文件，可任意中断重跑。

## 测试

```sh
go test ./...
```

故障注入通过环境变量 `PHOTOBATCH_CRASH_AT` 在子进程中触发（见 `crashhook.go`、`crash_test.go`）。
