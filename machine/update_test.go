package machine

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetFor(t *testing.T) {
	cases := []struct {
		goos, goarch, goarm, gomips string
		asset, binary               string
	}{
		{"linux", "amd64", "", "", "linux-64", "v2node"},
		{"linux", "arm64", "", "", "linux-arm64-v8a", "v2node"},
		{"linux", "arm", "7", "", "linux-arm32-v7a", "v2node"},
		{"linux", "arm", "6,softfloat", "", "linux-arm32-v6", "v2node"},
		{"linux", "mipsle", "", "softfloat", "linux-mips32le", "v2node_softfloat"},
		{"linux", "mips", "", "hardfloat", "linux-mips32", "v2node"},
		{"freebsd", "amd64", "", "", "freebsd-64", "v2node"},
	}
	for _, c := range cases {
		asset, binary, err := assetFor(c.goos, c.goarch, c.goarm, c.gomips)
		if err != nil || asset != c.asset || binary != c.binary {
			t.Errorf("%s/%s: %s %s %v", c.goos, c.goarch, asset, binary, err)
		}
	}
	if _, _, err := assetFor("windows", "amd64", "", ""); err == nil {
		t.Error("windows 不支持远程更新")
	}
}

func TestValidVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.4.9-gvboard.3": true, "v1.0.0-gvboard": true,
		"v0.4.9": false, "v0.4.9-gvboard.3/../x": false, "../v0.4.9-gvboard.3": false, "": false,
	} {
		if ValidVersion(v) != want {
			t.Errorf("ValidVersion(%q) 应为 %v", v, want)
		}
	}
}

func TestParseDigest(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, s := range []string{
		"MD5= 00\nSHA1= 11\nSHA2-256= " + sum + "\nSHA2-512= 22\n",
		"SHA256= " + strings.ToUpper(sum) + "\r\n",
	} {
		if got, err := parseDigest([]byte(s)); err != nil || got != sum {
			t.Errorf("parseDigest(%q) = %q %v", s, got, err)
		}
	}
	if _, err := parseDigest([]byte("MD5= 00\n")); err == nil {
		t.Error("没有 sha256 时应失败")
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstall(t *testing.T) {
	const version = "v0.4.9-gvboard.3"
	archive := makeZip(t, map[string]string{"v2node": "new-binary", "geoip.dat": "x"})
	sum := sha256.Sum256(archive)
	digest := "SHA2-256= " + hex.EncodeToString(sum[:]) + "\n"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/" + version + "/v2node-linux-64.zip":
			_, _ = w.Write(archive)
		case "/" + version + "/v2node-linux-64.zip.dgst":
			_, _ = w.Write([]byte(digest))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "v2node")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		b, _ := os.ReadFile(exe)
		return string(b)
	}
	var checked string
	u := &Updater{BaseURL: srv.URL + "/", Exe: exe, Asset: "linux-64", Binary: "v2node",
		Check: func(_ context.Context, path, v string) error {
			b, _ := os.ReadFile(path)
			checked = string(b) + "@" + v
			return nil
		}}

	if _, err := u.Install(context.Background(), "../../evil"); err == nil || len(paths) != 0 {
		t.Fatal("非法版本号不应发起下载")
	}
	got, err := u.Install(context.Background(), version)
	if err != nil || got != exe || read() != "new-binary" || checked != "new-binary@"+version {
		t.Fatalf("更新失败: %v %q %q", err, read(), checked)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("临时文件应已移除")
	}

	// 校验不一致、新程序无法运行、版本不存在时都不改动当前程序。
	_ = os.WriteFile(exe, []byte("old-binary"), 0o755)
	digest = "SHA2-256= " + strings.Repeat("0", 64) + "\n"
	if _, err := u.Install(context.Background(), version); err == nil || !strings.Contains(err.Error(), "sha256") || read() != "old-binary" {
		t.Fatalf("校验不一致应失败: %v", err)
	}
	digest = "SHA2-256= " + hex.EncodeToString(sum[:]) + "\n"
	u.Check = func(context.Context, string, string) error { return errors.New("新程序无法运行") }
	if _, err := u.Install(context.Background(), version); err == nil || read() != "old-binary" {
		t.Fatalf("新程序无法运行时应失败: %v", err)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("失败后临时文件应已移除")
	}
	if _, err := u.Install(context.Background(), "v0.4.9-gvboard.9"); err == nil || read() != "old-binary" {
		t.Fatal("版本不存在时应失败")
	}
	u.Binary = "v2node_softfloat"
	u.Check = nil
	if _, err := u.Install(context.Background(), version); err == nil || !strings.Contains(err.Error(), "v2node_softfloat") {
		t.Fatalf("安装包中没有程序时应失败: %v", err)
	}
}

func TestClientReport(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("ETag", `"1"`)
		_, _ = w.Write([]byte(`{"log_level":"info","pull_interval":60,"nodes":[{"node_id":1}],"update":{"version":"v0.4.9-gvboard.3","id":2}}`))
	}))
	defer srv.Close()
	c := &Client{Cred: &Credential{Panel: srv.URL, Token: "gvm_x"}, Version: "v0.4.9-gvboard.2"}
	c.Report(2, errors.New("下载失败"))
	cfg, _, err := c.Fetch(context.Background())
	if err != nil || cfg.Update == nil || *cfg.Update != (Update{Version: "v0.4.9-gvboard.3", ID: 2}) {
		t.Fatalf("解析更新指令失败: %+v %v", cfg, err)
	}
	if !strings.Contains(queries[0], "update_id=2") || !strings.Contains(queries[0], "update_error=") {
		t.Fatalf("应上报更新结果: %s", queries[0])
	}
	if _, _, err := c.Fetch(context.Background()); err != nil || strings.Contains(queries[1], "update_id") {
		t.Fatalf("上报成功后不应重复上报: %s %v", queries[1], err)
	}

	a := &Config{LogLevel: "info", Nodes: []NodeConfig{{NodeID: 1}}}
	b := &Config{LogLevel: "info", PullInterval: 30, Nodes: []NodeConfig{{NodeID: 1}}, Update: &Update{Version: "v0.4.9-gvboard.3", ID: 1}}
	if !a.SameNodes(b) || a.SameNodes(&Config{LogLevel: "debug", Nodes: a.Nodes}) || a.SameNodes(nil) {
		t.Fatal("SameNodes 判断不符")
	}
}
