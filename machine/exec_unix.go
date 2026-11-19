//go:build linux || freebsd

package machine

import (
	"os"
	"syscall"
)

// Restart 用新程序替换当前进程（进程号不变），启动参数与环境变量保持不变。
func Restart(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
