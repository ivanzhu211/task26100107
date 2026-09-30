# photobatch

屋面现场照片批次入库组件（Go 库）。批次状态存 SQLite，原图与缩略图存本地目录。

## 使用

```go
s, err := photobatch.Open("/var/lib/photos")
defer s.Close()

now := time.Now().UTC() // 调用方提供业务时间，UTC 且单调不减

// 接收：幂等键 key 固定工地/阶段/总数，编号 1..total
err = s.Receive(now, key, "工地A", "屋面施工", 10, 1, original, thumbnail)

// 查询：仅状态与已接收编号，不含照片内容
info, err := s.Query(now, key)

// 提交：全部编号收齐后整批公开；重复提交返回原结果
err = s.Submit(now, key)

// 读回：仅已提交批次可读，按编号 1..total 顺序
photos, err := s.ReadPhotos(key)

// 清理：删除过期批次与放弃接收留下的图片文件；过期键永久不可复用
err = s.Cleanup(now)
```

## 设计

- `state.db`（SQLite，WAL + FULL sync）：批次表（含首次接收时间、提交时间）、
  照片接收记录（内容 SHA-256）、单调时钟持久化。
- `photos/<keyHash>/<n>.orig|.thumb`：先写 `.tmp` 并 fsync，再 rename，最后 fsync 目录，
  崩溃后不会出现半文件；`.tmp` 与孤儿目录在 Open/Cleanup 时幂等回收。
- 接收登记、提交、清理的状态变更均为单事务；文件与库之间的崩溃残留
  未被确认，重试时按内容哈希调和，不产生重复记录或半张已接收照片。
- 未提交批次自首次接收起 24 小时到期（含恰好到达），到期即拒绝接收与提交；
  已提交批次不受期限影响；过期批次的行永久保留以识别迟到重放。

## 测试

```sh
go test ./...
```

覆盖：业务规则、部分落盘/提交中退出后原键重试（逐字节一致、无重复）、
截止前重启继续接收、到点整批过期、已提交跨过截止、清理中断后反复恢复与迟到重放。
