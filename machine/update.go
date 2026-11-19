package machine

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// 远程更新：面板只下发版本号，下载地址固定为本仓库的 Release，下载后按同一 Release 中的 .dgst 校验 sha256。

const releaseBase = "https://github.com/gunzi-666/v2node/releases/download/"

const (
	maxArchive = 256 << 20
	maxDigest  = 64 << 10
)

var versionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+-gvboard[0-9A-Za-z.\-]*$`)

// ValidVersion 是否为 gvboard 版 v2node 的版本号，其他内容一律拒绝。
func ValidVersion(v string) bool { return len(v) <= 64 && versionPattern.MatchString(v) }

// Update 面板下发的更新指令，同一 ID 只尝试一次。
type Update struct {
	Version string `json:"version"`
	ID      int64  `json:"id"`
}

// assetFor 与 .github/build/friendly-filenames.json 一致的安装包名，以及包内程序文件名。
func assetFor(goos, goarch, goarm, gomips string) (asset, binary string, err error) {
	binary = "v2node"
	names := map[string]string{
		"linux/amd64": "linux-64", "linux/386": "linux-32", "linux/arm64": "linux-arm64-v8a",
		"linux/riscv64": "linux-riscv64", "linux/mips64": "linux-mips64", "linux/mips64le": "linux-mips64le",
		"linux/mips": "linux-mips32", "linux/mipsle": "linux-mips32le", "linux/ppc64": "linux-ppc64",
		"linux/ppc64le": "linux-ppc64le", "linux/s390x": "linux-s390x",
		"freebsd/amd64": "freebsd-64", "freebsd/386": "freebsd-32", "freebsd/arm64": "freebsd-arm64-v8a",
	}
	key := goos + "/" + goarch
	if goarch == "arm" {
		v := goarm
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		if v == "" {
			v = "7"
		}
		switch {
		case goos == "linux" && v == "7":
			return "linux-arm32-v7a", binary, nil
		case goos == "linux" && (v == "5" || v == "6"):
			return "linux-arm32-v" + v, binary, nil
		case goos == "freebsd" && v == "7":
			return "freebsd-arm32-v7a", binary, nil
		}
	}
	asset, ok := names[key]
	if !ok {
		return "", "", fmt.Errorf("当前系统 %s 不支持远程更新，请登录服务器手动更新", key)
	}
	// mips 安装包内同时有 v2node 与 v2node_softfloat
	if (goarch == "mips" || goarch == "mipsle") && gomips == "softfloat" {
		binary = "v2node_softfloat"
	}
	return asset, binary, nil
}

// Asset 本机对应的安装包名与包内程序文件名。
func Asset() (asset, binary string, err error) {
	var goarm, gomips string
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "GOARM":
				goarm = s.Value
			case "GOMIPS":
				gomips = s.Value
			}
		}
	}
	return assetFor(runtime.GOOS, runtime.GOARCH, goarm, gomips)
}

// Updater 下载、校验并替换本机程序。字段为空时使用默认值，测试可替换。
type Updater struct {
	HTTP    *http.Client
	BaseURL string
	// Exe 要替换的程序路径，默认为当前程序。
	Exe string
	// Asset、Binary 安装包名与包内程序文件名，默认按本机系统与架构。
	Asset, Binary string
	// Check 校验新程序能否运行，默认执行「新程序 version」并核对版本号。
	Check func(ctx context.Context, path, version string) error
}

// Executable 当前程序的实际路径。
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	client := u.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败: %w", filepath.Base(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败: HTTP %d", filepath.Base(url), resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败: %w", filepath.Base(url), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s 超过大小限制", filepath.Base(url))
	}
	return b, nil
}

// parseDigest 从 .dgst 文件中取出 sha256，格式为「SHA2-256= 十六进制」或「SHA256= 十六进制」。
func parseDigest(b []byte) (string, error) {
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.ToUpper(strings.TrimSpace(k)), strings.ToLower(strings.TrimSpace(v))
		if (k == "SHA256" || k == "SHA2-256") && len(v) == 64 {
			if _, err := hex.DecodeString(v); err == nil {
				return v, nil
			}
		}
	}
	return "", errors.New("校验文件中没有 sha256")
}

// Install 下载指定版本、校验 sha256、确认新程序能运行后替换当前程序，返回被替换的程序路径。
// 任何一步失败都不改动当前程序。
func (u *Updater) Install(ctx context.Context, version string) (string, error) {
	if !ValidVersion(version) {
		return "", fmt.Errorf("版本号 %q 不是 gvboard 版 v2node", version)
	}
	asset, binary := u.Asset, u.Binary
	if asset == "" {
		var err error
		if asset, binary, err = Asset(); err != nil {
			return "", err
		}
	}
	exe := u.Exe
	if exe == "" {
		var err error
		if exe, err = Executable(); err != nil {
			return "", fmt.Errorf("获取当前程序路径失败: %w", err)
		}
	}
	base := u.BaseURL
	if base == "" {
		base = releaseBase
	}
	url := base + version + "/v2node-" + asset + ".zip"
	dgst, err := u.get(ctx, url+".dgst", maxDigest)
	if err != nil {
		return "", err
	}
	want, err := parseDigest(dgst)
	if err != nil {
		return "", err
	}
	data, err := u.get(ctx, url, maxArchive)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return "", errors.New("安装包 sha256 校验不一致")
	}
	bin, err := unzip(data, binary)
	if err != nil {
		return "", err
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return "", fmt.Errorf("写入新程序失败: %w", err)
	}
	check := u.Check
	if check == nil {
		check = checkVersion
	}
	if err := check(ctx, tmp, version); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("替换程序失败: %w", err)
	}
	return exe, nil
}

func unzip(data []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("解压安装包失败: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("解压安装包失败: %w", err)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxArchive+1))
		if err != nil {
			return nil, fmt.Errorf("解压安装包失败: %w", err)
		}
		if len(b) > maxArchive {
			return nil, errors.New("安装包内程序超过大小限制")
		}
		return b, nil
	}
	return nil, fmt.Errorf("安装包中没有 %s", name)
}

// checkVersion 执行「新程序 version」，输出中必须包含目标版本号。
func checkVersion(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("新程序无法运行: %w", err)
	}
	if !strings.Contains(string(out), version) {
		return fmt.Errorf("新程序版本不符: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
