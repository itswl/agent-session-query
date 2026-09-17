//go:build windows

package app

import "syscall"

// enableUTF8Console 把控制台输出代码页切成 UTF-8。
//
// 启动横幅、数据源路径、警告全是中文，而 Windows 传统控制台（cmd.exe）默认用本地
// 代码页（简中是 GBK/936），直接打 UTF-8 字节就是一屏乱码。Windows Terminal 和
// PowerShell 7 本来就是 UTF-8，这一步对它们是无操作。
//
// 拿不到控制台（输出被重定向到文件、或作为服务运行）时调用会失败，忽略即可——
// 那种场景下本来就没有代码页这回事。
func enableUTF8Console() {
	const cpUTF8 = 65001
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleOutputCP")
	if proc.Find() != nil {
		return
	}
	_, _, _ = proc.Call(uintptr(cpUTF8))
}
