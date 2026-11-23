package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// 一台节点机接入多个面板：每个面板一份凭据，保存在 DefaultDir 目录。

// DefaultDir 凭据目录，每个面板一个文件。
const DefaultDir = "/etc/v2node/machines"

// CertBase 各面板节点证书目录的上级目录，测试可替换。
var CertBase = "/etc/v2node/certs"

// Stored 已保存的凭据及其文件。
type Stored struct {
	Path string
	Cred *Credential
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9.\-]+`)

// Slug 由面板地址生成文件名（不含扩展名）：主机、端口与路径，非字母数字替换为 _。
func Slug(panel string) string {
	s := panel
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.Trim(unsafeChars.ReplaceAllString(s, "_"), "_.")
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		s = "panel"
	}
	return s
}

// CertDir 面板节点的证书目录。
func CertDir(panel string) string { return filepath.Join(CertBase, Slug(panel)) }

// scan 读取旧版凭据文件与目录中的 *.json，不去重。
func scan(dir, legacy string) (list []Stored, errs []error) {
	var paths []string
	if legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			paths = append(paths, legacy)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "*.json")); err == nil {
		slices.Sort(matches)
		paths = append(paths, matches...)
	}
	for _, p := range paths {
		c, err := Load(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		list = append(list, Stored{Path: p, Cred: c})
	}
	return list, errs
}

// LoadAll 读取全部凭据：目录中的 *.json 与旧版凭据文件 legacy。按接入时间排列，同一面板只保留最后接入的一份；
// 无法读取的文件返回在 errs 中，不影响其它凭据。
func LoadAll(dir, legacy string) (list []Stored, errs []error) {
	list, errs = scan(dir, legacy)
	slices.SortStableFunc(list, func(a, b Stored) int {
		switch {
		case a.Cred.EnrolledAt < b.Cred.EnrolledAt:
			return -1
		case a.Cred.EnrolledAt > b.Cred.EnrolledAt:
			return 1
		}
		return 0
	})
	out := list[:0]
	for i, s := range list {
		dup := false
		for _, later := range list[i+1:] {
			dup = dup || later.Cred.Panel == s.Cred.Panel
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out, errs
}

// Add 保存新接入的凭据：同一面板替换原凭据（replaced 为 true），不同面板新增；旧版凭据文件移入目录。
func Add(dir, legacy string, cred *Credential) (path string, replaced bool, err error) {
	list, _ := scan(dir, legacy)
	for _, s := range list {
		switch {
		case s.Cred.Panel == cred.Panel:
			replaced = true
		case s.Path != legacy:
			continue
		case !slices.ContainsFunc(list, func(o Stored) bool { return o.Path != legacy && o.Cred.Panel == s.Cred.Panel }):
			p, err := freePath(dir, s.Cred.Panel)
			if err != nil {
				return "", false, err
			}
			if err := Save(p, s.Cred); err != nil {
				return "", false, err
			}
		}
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	if path, err = freePath(dir, cred.Panel); err != nil {
		return "", false, err
	}
	return path, replaced, Save(path, cred)
}

// freePath 目录中该面板可用的文件名，与其它面板的文件重名时加序号。
func freePath(dir, panel string) (string, error) {
	base := Slug(panel)
	for i := 1; i < 100; i++ {
		name := base
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
		p := filepath.Join(dir, name+".json")
		c, err := Load(p)
		if errors.Is(err, os.ErrNotExist) || (err == nil && c.Panel == panel) {
			return p, nil
		}
	}
	return "", fmt.Errorf("无法为面板 %s 分配凭据文件名", panel)
}

// Remove 删除某个面板的凭据，返回删除的份数。
func Remove(dir, legacy, panel string) (int, error) {
	panel, err := NormalizePanel(panel)
	if err != nil {
		return 0, err
	}
	n := 0
	list, _ := scan(dir, legacy)
	for _, s := range list {
		if s.Cred.Panel != panel {
			continue
		}
		if err := os.Remove(s.Path); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// NodeError 被跳过的节点（端口冲突、启动失败）与原因，上报给该节点所属的面板。
type NodeError struct {
	NodeID int    `json:"node_id"`
	Error  string `json:"error"`
}

// ReportNodes 向面板上报本面板被跳过的节点，空列表表示全部恢复；与上次成功上报的相同时不请求。
// 面板不支持该接口（404）时视为已上报，避免旧版面板上反复请求。
func (c *Client) ReportNodes(ctx context.Context, list []NodeError) error {
	if list == nil {
		list = []NodeError{}
	}
	body, err := json.Marshal(map[string]any{"errors": list})
	if err != nil {
		return err
	}
	if c.nodesOK && c.nodesSent == string(body) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Cred.Panel+"/api/v2/server/machine/node-errors", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Cred.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		c.nodesSent, c.nodesOK = string(body), true
		return nil
	}
	return fmt.Errorf("面板返回 %d", resp.StatusCode)
}

// Newer 版本 a 是否比 b 新（v主.次.修订-gvboard.N 格式），b 无法识别时视为 a 更新，a 无法识别时为 false。
func Newer(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	for i := 0; i < max(len(pa), len(pb)); i++ {
		if i >= len(pb) {
			return true
		}
		if i >= len(pa) {
			return false
		}
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// parseVersion 把 v0.4.9-gvboard.3 解析为 [0 4 9 3]；gvboard 后的非数字部分无法比较时返回 false。
func parseVersion(v string) ([]int, bool) {
	if !ValidVersion(v) {
		return nil, false
	}
	main, suffix, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-gvboard")
	var out []int
	for _, s := range strings.Split(main, ".") {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	suffix = strings.Trim(suffix, ".-")
	if suffix == "" {
		return out, true
	}
	for _, s := range strings.FieldsFunc(suffix, func(r rune) bool { return r == '.' || r == '-' }) {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
