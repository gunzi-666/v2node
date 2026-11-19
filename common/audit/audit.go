package audit

import (
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

// 规则动作，命中多条规则时按 封禁 > 阻断 > 记录 处理。
const (
	ActionLog   = "log"
	ActionBlock = "block"
	ActionBan   = "ban"
)

const (
	// BanDuration 命中封禁规则后在本节点阻断该用户的时长，面板随后封禁整个账号。
	BanDuration = 10 * time.Minute
	// maxPending 待上报记录的上限（按 用户、规则、目标、网络 去重后的条数），超出的新记录丢弃。
	maxPending = 10000
)

func severity(action string) int {
	switch action {
	case ActionBan:
		return 3
	case ActionBlock:
		return 2
	case ActionLog:
		return 1
	}
	return 0
}

type rule struct {
	id      int64
	action  string
	entries []*entry
}

type recordKey struct {
	uid     int
	rule    int64
	target  string
	network string
}

// Auditor 一个节点的审计规则、待上报记录与本地封禁。
type Auditor struct {
	tag     string
	rules   atomic.Pointer[[]*rule]
	mu      sync.Mutex
	pending map[recordKey]*panel.AuditRecord
	bans    map[int]time.Time
	now     func() time.Time
}

var registry sync.Map // 节点 tag → *Auditor

// Register 为节点创建审计器（已存在时返回原有的）。
func Register(tag string) *Auditor {
	a := &Auditor{tag: tag, pending: map[recordKey]*panel.AuditRecord{}, bans: map[int]time.Time{}, now: time.Now}
	v, _ := registry.LoadOrStore(tag, a)
	return v.(*Auditor)
}

// Get 返回节点的审计器，节点未开启审计时为 nil。
func Get(tag string) *Auditor {
	if v, ok := registry.Load(tag); ok {
		return v.(*Auditor)
	}
	return nil
}

// Remove 删除节点的审计器。
func Remove(tag string) {
	registry.Delete(tag)
}

// SetRules 替换审计规则，无法解析的匹配内容与未知动作跳过并记录日志。
func (a *Auditor) SetRules(list []panel.AuditRule) {
	rules := make([]*rule, 0, len(list))
	lines := 0
	for _, r := range list {
		if severity(r.Action) == 0 {
			log.WithFields(log.Fields{"tag": a.tag, "rule": r.ID, "action": r.Action}).Warn("Unknown audit action, skipped")
			continue
		}
		nr := &rule{id: r.ID, action: r.Action}
		for _, m := range r.Match {
			e, err := parseLine(m)
			if err != nil {
				log.WithFields(log.Fields{"tag": a.tag, "rule": r.ID, "match": m, "err": err}).Warn("Invalid audit match, skipped")
				continue
			}
			nr.entries = append(nr.entries, e)
		}
		if len(nr.entries) > 0 {
			rules = append(rules, nr)
			lines += len(nr.entries)
		}
	}
	a.rules.Store(&rules)
	log.WithField("tag", a.tag).Infof("Loaded %d audit rules (%d matches)", len(rules), lines)
}

// Check 检查一次访问，返回是否应阻断。targets 为同一连接的不同写法（如原始目标与嗅探到的域名），
// 命中多条规则时取最严重的动作。命中即记录，封禁动作同时在本节点封禁该用户 BanDuration。
func (a *Auditor) Check(uid int, srcIP, network string, targets ...Target) bool {
	now := a.now()
	a.mu.Lock()
	if until, ok := a.bans[uid]; ok {
		if now.Before(until) {
			a.mu.Unlock()
			return true
		}
		delete(a.bans, uid)
	}
	a.mu.Unlock()

	rp := a.rules.Load()
	if rp == nil || len(*rp) == 0 {
		return false
	}
	var best *rule
	var hit Target
	for _, r := range *rp {
		if best != nil && severity(r.action) <= severity(best.action) {
			continue
		}
		for _, t := range targets {
			if r.matches(t) {
				best, hit = r, t
				break
			}
		}
		if best != nil && best.action == ActionBan {
			break
		}
	}
	if best == nil {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if best.action == ActionBan {
		a.bans[uid] = now.Add(BanDuration)
	}
	key := recordKey{uid: uid, rule: best.id, target: hit.String(), network: network}
	if rec, ok := a.pending[key]; ok {
		rec.Count++
		rec.Time, rec.IP, rec.Action = now.Unix(), srcIP, best.action
	} else if len(a.pending) < maxPending {
		a.pending[key] = &panel.AuditRecord{
			UserID: uid, RuleID: best.id, Action: best.action, Target: key.target,
			Network: network, IP: srcIP, Count: 1, Time: now.Unix(),
		}
	}
	return best.action != ActionLog
}

func (r *rule) matches(t Target) bool {
	for _, e := range r.entries {
		if e.match(t) {
			return true
		}
	}
	return false
}

// Flush 取出全部待上报记录。
func (a *Auditor) Flush() []panel.AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) == 0 {
		return nil
	}
	out := make([]panel.AuditRecord, 0, len(a.pending))
	for _, r := range a.pending {
		out = append(out, *r)
	}
	a.pending = map[recordKey]*panel.AuditRecord{}
	for uid, until := range a.bans {
		if !a.now().Before(until) {
			delete(a.bans, uid)
		}
	}
	return out
}

// Restore 上报失败时放回记录，与期间新增的记录合并，超过上限的丢弃。
func (a *Auditor) Restore(records []panel.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range records {
		key := recordKey{uid: r.UserID, rule: r.RuleID, target: r.Target, network: r.Network}
		if cur, ok := a.pending[key]; ok {
			cur.Count += r.Count
		} else if len(a.pending) < maxPending {
			rec := r
			a.pending[key] = &rec
		}
	}
}
