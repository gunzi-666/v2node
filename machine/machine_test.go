package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnrollFetch(t *testing.T) {
	revoked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/enroll":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["code"] != "good" || in["version"] != "v1" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"machine_id":7,"token":"gvm_abc"}`))
		case "/api/v2/server/machine/config":
			if revoked || r.Header.Get("Authorization") != "Bearer gvm_abc" || r.URL.Query().Get("version") != "v1" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("ETag", `"e1"`)
			if r.Header.Get("If-None-Match") == `"e1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write([]byte(`{"log_level":"debug","pull_interval":30,"nodes":[{"node_id":1},{"node_id":2,"audit_enable":true}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	if _, err := Enroll(ctx, srv.URL, "bad", "h", "v1"); err == nil {
		t.Fatal("错误接入码应失败")
	}
	if _, err := Enroll(ctx, "ftp://x", "good", "h", "v1"); err == nil {
		t.Fatal("非 http 地址应失败")
	}
	cred, err := Enroll(ctx, srv.URL+"/", "good", "h", "v1")
	if err != nil || cred.Panel != srv.URL || cred.MachineID != 7 || cred.Token != "gvm_abc" {
		t.Fatalf("接入结果不符: %+v %v", cred, err)
	}

	path := filepath.Join(t.TempDir(), "sub", "machine.json")
	if err := Save(path, cred); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("凭据文件权限不符: %v %v", st, err)
	}
	loaded, err := Load(path)
	if err != nil || *loaded != *cred {
		t.Fatalf("读取凭据不符: %+v %v", loaded, err)
	}

	c := &Client{Cred: loaded, Version: "v1"}
	cfg, changed, err := c.Fetch(ctx)
	if err != nil || !changed || cfg.PullInterval != 30 || len(cfg.Nodes) != 2 {
		t.Fatalf("首次拉取不符: %+v %v %v", cfg, changed, err)
	}
	cc := cfg.Conf(loaded)
	if cc.LogConfig.Level != "debug" || len(cc.NodeConfigs) != 2 || cc.NodeConfigs[1].NodeID != 2 || !cc.NodeConfigs[1].AuditEnable ||
		cc.NodeConfigs[0].AuditEnable || cc.NodeConfigs[0].APIHost != srv.URL || cc.NodeConfigs[0].Key != "gvm_abc" {
		t.Fatalf("转换配置不符: %+v", cc)
	}
	if again, changed, err := c.Fetch(ctx); err != nil || changed || again != cfg {
		t.Fatalf("ETag 未生效: %v %v", changed, err)
	}
	c.Reset()
	if _, changed, _ := c.Fetch(ctx); !changed {
		t.Fatal("Reset 后应返回完整配置")
	}
	revoked = true
	if _, _, err := c.Fetch(ctx); !errors.Is(err, ErrRevoked) {
		t.Fatalf("吊销后应返回 ErrRevoked: %v", err)
	}
}
