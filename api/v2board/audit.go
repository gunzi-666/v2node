package panel

import (
	"context"
	"fmt"
	"net/http"

	"encoding/json/v2"
)

// AuditRule 面板下发的审计规则（Gvboard），Match 每项一条匹配内容。
type AuditRule struct {
	ID     int64    `json:"id"`
	Action string   `json:"action"`
	Match  []string `json:"match"`
}

// AuditRecord 上报给面板的审计命中记录，UserID 为节点侧的用户 ID。
type AuditRecord struct {
	UserID  int    `json:"user_id"`
	RuleID  int64  `json:"rule_id"`
	Action  string `json:"action"`
	Target  string `json:"target"`
	Network string `json:"network"`
	IP      string `json:"ip"`
	Count   int64  `json:"count"`
	Time    int64  `json:"time"`
}

const auditPath = "/api/v1/server/UniProxy/audit"

// GetAuditRules 拉取审计规则。规则未变化（304）时 changed 为 false；
// 面板不支持审计（404）时视为没有规则。
func (c *Client) GetAuditRules(ctx context.Context) (rules []AuditRule, changed bool, err error) {
	r, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.auditEtag).
		ForceContentType("application/json").
		Get(auditPath)
	if err != nil {
		return nil, false, err
	}
	switch r.StatusCode() {
	case http.StatusNotModified:
		return nil, false, nil
	case http.StatusNotFound:
		c.auditEtag = ""
		return []AuditRule{}, true, nil
	}
	if r.StatusCode() >= 400 {
		return nil, false, fmt.Errorf("get audit rules: status %d", r.StatusCode())
	}
	var body struct {
		Rules []AuditRule `json:"rules"`
	}
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		return nil, false, fmt.Errorf("decode audit rules error: %w", err)
	}
	c.auditEtag = r.Header().Get("ETag")
	if body.Rules == nil {
		body.Rules = []AuditRule{}
	}
	return body.Rules, true, nil
}

// ReportAudit 上报审计命中记录。
func (c *Client) ReportAudit(ctx context.Context, records []AuditRecord) error {
	b, err := json.Marshal(map[string]any{"records": records})
	if err != nil {
		return err
	}
	r, err := c.client.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetBody(b).
		Post(auditPath)
	if err != nil {
		return err
	}
	if r.StatusCode() >= 400 {
		return fmt.Errorf("report audit: status %d", r.StatusCode())
	}
	return nil
}
