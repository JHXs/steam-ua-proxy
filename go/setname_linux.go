//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// setProcessName 设置 comm（Linux PR_SET_NAME）。
//
// mihomo 在 Linux 上按 /proc/<pid>/exe 匹配进程，这里只是让 ps/top 看着清楚；
// 真正让 PROCESS-NAME 命中的是可执行文件名本身。
func setProcessName(name string) {
	if len(name) > 15 { // comm 最多 15 字节
		name = name[:15]
	}
	b := append([]byte(name), 0)
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, 15, uintptr(unsafe.Pointer(&b[0])), 0)
	if errno != 0 {
		fmt.Fprintln(os.Stderr, "设置进程名失败:", errno)
	}
}
