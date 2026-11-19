//go:build !linux && !freebsd

package machine

import "errors"

// Restart 当前系统不支持远程更新。
func Restart(string) error {
	return errors.New("当前系统不支持远程更新后自动重启")
}
