package photobatch

import "os"

// crashPoint 是故障注入点，仅供自动化测试模拟进程被直接终止。
// 设置环境变量 PHOTOBATCH_CRASH_AT 为指定名称后，到达该位置时进程立即退出。
// 正常使用时环境变量为空，开销可忽略。
func crashPoint(name string) {
	if os.Getenv("PHOTOBATCH_CRASH_AT") == name {
		os.Exit(1)
	}
}
