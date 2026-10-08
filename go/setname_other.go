//go:build !linux

package main

// setProcessName 在非 Linux 平台是空实现。
//
// Windows 上 mihomo 按进程映像名匹配（即 `steam-ua-proxy.exe`），
// 进程名本来就等于可执行文件名，不需要额外设置。
func setProcessName(string) {}
