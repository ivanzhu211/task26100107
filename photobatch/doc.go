// Package photobatch 实现屋面现场照片的批次入库。
//
// 施工人员离线拍摄后集中传回照片；本组件以幂等键接收照片对
// （原图 + 300px 缩略图），批次状态存 SQLite，图片存本地目录。
//
// 基本流程：
//
//	s, _ := photobatch.Open(dir)
//	s.Receive(now, key, site, stage, total, no, original, thumbnail) // 逐张接收
//	s.Query(now, key)                                                // 查状态与已收编号
//	s.Submit(now, key)                                               // 收齐后提交，整批公开
//	photos, _ := s.ReadPhotos(key)                                   // 读回整批照片
//	s.Cleanup(now)                                                   // 清理过期批次文件
//
// 规则要点：
//   - 幂等键全局唯一，固定工地、阶段与总数（1..50），编号 1..total 连续。
//   - 相同内容重复接收视为同一次；内容不同拒绝，不覆盖已接收数据。
//   - 业务时间由调用方提供，UTC 且单调不减；未提交批次自首次接收起
//     24 小时到期，到期即不可再接收或提交；已提交批次不受期限影响。
//   - 过期键永久不可复用；清理只删除过期与放弃接收留下的图片文件。
//   - 单进程串行调用；进程可在任意时刻被终止，重启后 Open 自动恢复。

package photobatch
