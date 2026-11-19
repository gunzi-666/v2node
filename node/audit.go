package node

import (
	"context"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wyx2685/v2node/common/audit"
)

// syncAuditRules 从面板拉取审计规则，有变化时替换。
func (c *Controller) syncAuditRules(ctx context.Context) error {
	rules, changed, err := c.apiClient.GetAuditRules(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{"tag": c.tag, "err": err}).Error("Get audit rules failed")
		return nil
	}
	if changed {
		c.auditor.SetRules(rules)
	}
	return nil
}

// reportAudit 上报审计命中记录，失败时放回等待下次上报。
func (c *Controller) reportAudit(ctx context.Context) error {
	records := c.auditor.Flush()
	if len(records) == 0 {
		return nil
	}
	if err := c.apiClient.ReportAudit(ctx, records); err != nil {
		c.auditor.Restore(records)
		log.WithFields(log.Fields{"tag": c.tag, "err": err}).Info("Report audit records failed")
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return nil
	}
	log.WithField("tag", c.tag).Infof("Report %d audit records", len(records))
	return nil
}

// startAudit 节点开启审计时注册审计器并拉取一次规则。
func (c *Controller) startAudit() {
	if !c.conf.AuditEnable {
		return
	}
	c.auditor = audit.Register(c.tag)
	_ = c.syncAuditRules(context.Background())
}

// closeAudit 尽量上报剩余的审计记录后注销审计器。
func (c *Controller) closeAudit() {
	if c.auditor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.reportAudit(ctx)
	audit.Remove(c.tag)
	c.auditor = nil
}
