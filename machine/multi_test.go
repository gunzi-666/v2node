package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"https://panel.example.com":          "panel.example.com",
		"https://panel.example.com:8443/sub": "panel.example.com_8443_sub",
		"http://1.2.3.4:80":                  "1.2.3.4_80",
		"https://":                           "panel",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q，应为 %q", in, got, want)
		}
	}
	old := CertBase
	CertBase = "/certs"
	defer func() { CertBase = old }()
	cfg := &Config{Nodes: []NodeConfig{{NodeID: 1}}}
	if got := cfg.Conf(&Credential{Panel: "https://a.com", Token: "t"}).NodeConfigs[0].CertDir; got != filepath.Join("/certs", "a.com") {
		t.Fatalf("证书目录不符: %q", got)
	}
}

func TestMultiPanel(t *testing.T) {
	root := t.TempDir()
	dir, legacy := filepath.Join(root, "machines"), filepath.Join(root, "machine.json")
	a1 := &Credential{Panel: "https://a.com", MachineID: 1, Token: "gvm_a1", EnrolledAt: 100}

	// 旧版凭据文件仍然读取。
	if err := Save(legacy, a1); err != nil {
		t.Fatal(err)
	}
	list, errs := LoadAll(dir, legacy)
	if len(errs) != 0 || len(list) != 1 || list[0].Path != legacy || *list[0].Cred != *a1 {
		t.Fatalf("读取旧版凭据不符: %+v %v", list, errs)
	}

	// 接入另一个面板：新增，旧版凭据移入目录。
	b := &Credential{Panel: "https://b.com:8443", MachineID: 9, Token: "gvm_b", EnrolledAt: 200}
	path, replaced, err := Add(dir, legacy, b)
	if err != nil || replaced || path != filepath.Join(dir, "b.com_8443.json") {
		t.Fatalf("新增面板不符: %q %v %v", path, replaced, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("旧版凭据应移入目录")
	}
	list, _ = LoadAll(dir, legacy)
	if len(list) != 2 || list[0].Cred.Panel != a1.Panel || list[0].Path != filepath.Join(dir, "a.com.json") || list[1].Cred.Panel != b.Panel {
		t.Fatalf("两个面板应按接入时间排列: %+v", list)
	}

	// 再次接入同一面板：替换原凭据。
	a2 := &Credential{Panel: "https://a.com", MachineID: 2, Token: "gvm_a2", EnrolledAt: 300}
	if path, replaced, err = Add(dir, legacy, a2); err != nil || !replaced || path != filepath.Join(dir, "a.com.json") {
		t.Fatalf("替换同一面板不符: %q %v %v", path, replaced, err)
	}
	list, _ = LoadAll(dir, legacy)
	if len(list) != 2 || list[0].Cred.Panel != b.Panel || *list[1].Cred != *a2 {
		t.Fatalf("替换后不符: %+v", list)
	}

	// 文件名与其它面板相同时加序号，不覆盖对方。
	c := &Credential{Panel: "https://a.com/", Token: "gvm_c", EnrolledAt: 400}
	if path, _, err = Add(dir, legacy, c); err != nil || path != filepath.Join(dir, "a.com-2.json") {
		t.Fatalf("重名时应加序号: %q %v", path, err)
	}
	if got, err := Load(filepath.Join(dir, "a.com.json")); err != nil || *got != *a2 {
		t.Fatal("重名时不应覆盖其它面板的凭据")
	}

	// 旧版凭据与目录中是同一面板时，旧文件直接删除。
	if err := Save(legacy, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Add(dir, legacy, &Credential{Panel: "https://d.com", Token: "gvm_d", EnrolledAt: 500}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("重复的旧版凭据应删除")
	}

	// 损坏的文件不影响其它凭据。
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, errs = LoadAll(dir, legacy)
	if len(list) != 4 || len(errs) != 1 {
		t.Fatalf("损坏文件处理不符: %d %v", len(list), errs)
	}

	// 移除。
	if n, err := Remove(dir, legacy, "https://b.com:8443/"); err != nil || n != 1 {
		t.Fatalf("移除失败: %d %v", n, err)
	}
	if n, err := Remove(dir, legacy, "https://none.com"); err != nil || n != 0 {
		t.Fatalf("移除不存在的面板: %d %v", n, err)
	}
	if _, err := Remove(dir, legacy, "ftp://x"); err == nil {
		t.Fatal("非法地址应失败")
	}
	list, _ = LoadAll(dir, legacy)
	for _, s := range list {
		if s.Cred.Panel == b.Panel {
			t.Fatal("移除后不应再读取到")
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.4.9-gvboard.4", "v0.4.9-gvboard.3", true},
		{"v0.4.9-gvboard.3", "v0.4.9-gvboard.4", false},
		{"v0.4.9-gvboard.3", "v0.4.9-gvboard.3", false},
		{"v0.4.10-gvboard", "v0.4.9-gvboard.9", true},
		{"v0.4.9-gvboard.1", "v0.4.9-gvboard", true},
		{"v0.4.9-gvboard", "v0.4.9-gvboard.1", false},
		{"v1.0.0-gvboard", "v0.9.9-gvboard.9", true},
		{"v0.4.9-gvboard.3", "dev", true},
		{"latest", "v0.4.9-gvboard.3", false},
		{"v0.4.9-gvboard.rc", "v0.4.9-gvboard.3", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v，应为 %v", c.a, c.b, got, c.want)
		}
	}
}

func TestReportNodes(t *testing.T) {
	var got []string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/server/machine/node-errors" || r.Header.Get("Authorization") != "Bearer gvm_x" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Errors []NodeError `json:"errors"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		b, _ := json.Marshal(in.Errors)
		got = append(got, string(b))
		w.WriteHeader(status)
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &Client{Cred: &Credential{Panel: srv.URL, Token: "gvm_x"}}

	if err := c.ReportNodes(ctx, nil); err != nil || len(got) != 1 || got[0] != "[]" {
		t.Fatalf("空列表应上报为 []: %v %v", got, err)
	}
	if err := c.ReportNodes(ctx, []NodeError{}); err != nil || len(got) != 1 {
		t.Fatal("没有变化时不应再上报")
	}
	list := []NodeError{{NodeID: 3, Error: "端口 443/TCP 与本面板节点 #1 冲突，已跳过"}}
	if err := c.ReportNodes(ctx, list); err != nil || len(got) != 2 || got[1] != `[{"node_id":3,"error":"端口 443/TCP 与本面板节点 #1 冲突，已跳过"}]` {
		t.Fatalf("上报内容不符: %v %v", got, err)
	}
	status = http.StatusInternalServerError
	if err := c.ReportNodes(ctx, nil); err == nil {
		t.Fatal("面板出错时应返回错误，下次重试")
	}
	status = http.StatusNoContent
	if err := c.ReportNodes(ctx, nil); err != nil || len(got) != 4 {
		t.Fatalf("失败后应重试: %v %v", got, err)
	}
	// 旧版面板没有该接口（404）时视为已上报。
	old := &Client{Cred: &Credential{Panel: srv.URL, Token: "gvm_other"}}
	if err := old.ReportNodes(ctx, list); err != nil {
		t.Fatalf("404 应视为已上报: %v", err)
	}
	if err := old.ReportNodes(ctx, list); err != nil {
		t.Fatal(err)
	}
}
