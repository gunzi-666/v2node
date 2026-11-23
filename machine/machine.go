// Package machine 节点机模式（配合 Gvboard 面板）：用一次性接入码换取本机凭据，节点配置由面板下发。
package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wyx2685/v2node/conf"
)

// DefaultPath 旧版（只能接入一个面板）的凭据文件，仍然读取，下次接入时移入 DefaultDir。
const DefaultPath = "/etc/v2node/machine.json"

// ErrRevoked 面板返回 404：凭据已吊销、节点机已删除或面板关闭了节点机。
var ErrRevoked = errors.New("节点机凭据无效或已被吊销")

// Credential 本机凭据。
type Credential struct {
	Panel     string `json:"panel"`
	MachineID int64  `json:"machine_id"`
	Token     string `json:"token"`
	// EnrolledAt 接入时间（Unix 秒），多个面板按接入先后排列；旧版凭据没有该字段。
	EnrolledAt int64 `json:"enrolled_at,omitempty"`
}

// Load 读取凭据文件。
func Load(path string) (*Credential, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if c.Panel == "" || c.Token == "" {
		return nil, fmt.Errorf("%s 缺少 panel 或 token", path)
	}
	return &c, nil
}

// Save 写入凭据文件，只有 root 可读。
func Save(path string, c *Credential) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NormalizePanel 去掉面板地址末尾的 /，并要求是 http(s) 地址。
func NormalizePanel(panel string) (string, error) {
	panel = strings.TrimRight(strings.TrimSpace(panel), "/")
	u, err := url.Parse(panel)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("面板地址必须是 http:// 或 https:// 开头")
	}
	return panel, nil
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Enroll 用接入码向面板换取凭据。
func Enroll(ctx context.Context, panel, code, hostname, version string) (*Credential, error) {
	panel, err := NormalizePanel(panel)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"code": strings.TrimSpace(code), "hostname": hostname, "version": version})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, panel+"/api/v2/server/machine/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接面板失败: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errors.New("接入码无效、已过期或已使用，或面板未开启节点机（请在面板重新生成安装命令）")
	default:
		return nil, fmt.Errorf("面板返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		MachineID int64  `json:"machine_id"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		return nil, fmt.Errorf("面板返回内容无法识别: %s", strings.TrimSpace(string(b)))
	}
	return &Credential{Panel: panel, MachineID: out.MachineID, Token: out.Token, EnrolledAt: time.Now().Unix()}, nil
}

// NodeConfig 面板下发的单个节点。
type NodeConfig struct {
	NodeID      int  `json:"node_id"`
	AuditEnable bool `json:"audit_enable"`
}

// Config 面板下发的配置。
type Config struct {
	LogLevel     string       `json:"log_level"`
	PullInterval int          `json:"pull_interval"`
	Nodes        []NodeConfig `json:"nodes"`
	// Update 面板要求更新到的版本，没有时为 nil。
	Update *Update `json:"update,omitempty"`
}

// SameNodes 日志级别与节点相同，只有拉取间隔或更新指令变化时不需要重建节点。
func (c *Config) SameNodes(o *Config) bool {
	return c != nil && o != nil && c.LogLevel == o.LogLevel && slices.Equal(c.Nodes, o.Nodes)
}

// Conf 转换为与配置文件等价的 conf.Conf：节点接口地址为面板、密钥为本机凭据，证书放在该面板自己的目录。
func (c *Config) Conf(cred *Credential) *conf.Conf {
	out := conf.New()
	if c.LogLevel != "" {
		out.LogConfig.Level = c.LogLevel
	}
	retry := conf.DefaultNodeRetryCount
	for _, n := range c.Nodes {
		out.NodeConfigs = append(out.NodeConfigs, conf.NodeConfig{
			APIHost: cred.Panel, NodeID: n.NodeID, Key: cred.Token, Timeout: conf.DefaultNodeTimeout,
			RetryCount: &retry, AuditEnable: n.AuditEnable, CertDir: CertDir(cred.Panel),
		})
	}
	return out
}

// Client 拉取本机配置，支持 ETag。
type Client struct {
	Cred    *Credential
	Version string
	etag    string
	last    *Config
	// 待上报的更新失败原因，面板收到后清除。
	reportID  int64
	reportErr string
	// 上次成功上报的被跳过节点（JSON），相同时不再上报。
	nodesSent string
	nodesOK   bool
}

// Reset 下次拉取时不带 ETag，强制返回完整配置。
func (c *Client) Reset() { c.etag = "" }

// Report 下次拉取配置时向面板上报更新失败的原因。
func (c *Client) Report(id int64, err error) {
	msg := []rune(err.Error())
	if len(msg) > 200 {
		msg = msg[:200]
	}
	c.reportID, c.reportErr = id, string(msg)
}

// Fetch 拉取配置。changed 为 false 表示与上次相同；面板返回 404 时返回 ErrRevoked。
func (c *Client) Fetch(ctx context.Context) (cfg *Config, changed bool, err error) {
	q := url.Values{"version": {c.Version}}
	if c.reportID != 0 {
		q.Set("update_id", strconv.FormatInt(c.reportID, 10))
		q.Set("update_error", c.reportErr)
	}
	u := c.Cred.Panel + "/api/v2/server/machine/config?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Cred.Token)
	if c.etag != "" && c.last != nil {
		req.Header.Set("If-None-Match", c.etag)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified {
		c.reportID, c.reportErr = 0, ""
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return c.last, false, nil
	case http.StatusNotFound:
		return nil, false, ErrRevoked
	default:
		return nil, false, fmt.Errorf("面板返回 %d", resp.StatusCode)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false, fmt.Errorf("解析节点机配置失败: %w", err)
	}
	c.etag, c.last = resp.Header.Get("ETag"), &out
	return &out, true, nil
}
