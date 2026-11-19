package audit

import (
	"net/netip"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func domain(d string, port uint16) Target { return Target{Domain: d, Port: port} }
func ip(s string, port uint16) Target     { return Target{IP: netip.MustParseAddr(s), Port: port} }

func TestParseLine(t *testing.T) {
	for _, ok := range []string{
		"example.com", "*.example.com", "full:a.example.com:443", "keyword:torrent", "REGEXP:^dl\\d+",
		"1.2.3.4", "10.0.0.0/8:25", "[2001:db8::1]:443", "2001:db8::/32", ":25", ":6881-6889", "a.com:1000-2000",
	} {
		if _, err := parseLine(ok); err != nil {
			t.Errorf("%q 应解析成功: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a.com:", "a.com:0", "a.com:20-10", ":x", "exa mple.com", "full:", "full::25", "keyword:", "regexp:(", "[::1", "10.0.0.0/33"} {
		if _, err := parseLine(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		line   string
		target Target
		want   bool
	}{
		{"example.com", domain("www.example.com", 443), true},
		{"example.com", domain("badexample.com", 443), false},
		{"example.com", ip("1.2.3.4", 443), false},
		{"full:only.test", domain("a.only.test", 80), false},
		{"keyword:torrent", domain("my-torrent.site", 80), true},
		{"regexp:^dl\\d+\\.", domain("dl12.cdn.net", 80), true},
		{"1.2.3.0/24:25", ip("1.2.3.9", 25), true},
		{"1.2.3.0/24:25", ip("1.2.3.9", 26), false},
		{"1.2.3.4", ip("::ffff:1.2.3.4", 80), true},
		{"[2001:db8::1]:443", ip("2001:db8::1", 443), true},
		{":6881-6889", domain("x.com", 6885), true},
		{":6881-6889", ip("9.9.9.9", 6890), false},
		{"smtp.test:465", domain("smtp.test", 25), false},
	}
	for _, c := range cases {
		e, err := parseLine(c.line)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.match(c.target); got != c.want {
			t.Errorf("%s 匹配 %v 期望 %v，实际 %v", c.line, c.target, c.want, got)
		}
	}
}

func TestTargetString(t *testing.T) {
	for want, tg := range map[string]Target{
		"a.com:443":         domain("a.com", 443),
		"1.2.3.4:25":        ip("1.2.3.4", 25),
		"[2001:db8::1]:443": ip("2001:db8::1", 443),
		"2001:db8::1":       ip("2001:db8::1", 0),
	} {
		if got := tg.String(); got != want {
			t.Errorf("期望 %s，实际 %s", want, got)
		}
	}
}

func TestAuditor(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := Register("test-node")
	defer Remove("test-node")
	a.now = func() time.Time { return now }
	if Get("test-node") != a || Register("test-node") != a {
		t.Fatal("注册表不符")
	}

	if a.Check(1, "1.1.1.1", "tcp", domain("bad.com", 443)) {
		t.Fatal("没有规则时不应阻断")
	}
	a.SetRules([]panel.AuditRule{
		{ID: 1, Action: "log", Match: []string{"example.com", "bad.com"}},
		{ID: 2, Action: "block", Match: []string{":25", "not a domain"}},
		{ID: 3, Action: "ban", Match: []string{"bad.com"}},
		{ID: 4, Action: "drop", Match: []string{"x.com"}},
		{ID: 5, Action: "block", Match: []string{"???"}},
	})
	if n := len(*a.rules.Load()); n != 3 {
		t.Fatalf("应加载 3 条规则，实际 %d", n)
	}

	// 只记录：放行；同一目标去重计数
	if a.Check(1, "1.1.1.1", "tcp", domain("www.example.com", 443)) || a.Check(1, "1.1.1.2", "tcp", domain("www.example.com", 443)) {
		t.Fatal("记录动作不应阻断")
	}
	// 嗅探域名与原始 IP 两种写法，按 IP 端口命中阻断
	if !a.Check(2, "2.2.2.2", "tcp", domain("mail.x.com", 25), ip("9.9.9.9", 25)) {
		t.Fatal("应阻断")
	}
	recs := a.Flush()
	if len(recs) != 2 {
		t.Fatalf("应有 2 条记录: %+v", recs)
	}
	for _, r := range recs {
		switch r.UserID {
		case 1:
			if r.RuleID != 1 || r.Count != 2 || r.IP != "1.1.1.2" || r.Target != "www.example.com:443" || r.Action != "log" {
				t.Fatalf("记录不符: %+v", r)
			}
		case 2:
			if r.RuleID != 2 || r.Target != "mail.x.com:25" || r.Action != "block" {
				t.Fatalf("记录不符: %+v", r)
			}
		}
	}
	if a.Flush() != nil {
		t.Fatal("取出后应清空")
	}

	// 同时命中记录与封禁：取封禁，并在本节点封禁 10 分钟
	if !a.Check(3, "3.3.3.3", "udp", domain("bad.com", 443)) {
		t.Fatal("封禁应阻断")
	}
	if !a.Check(3, "3.3.3.3", "tcp", domain("good.com", 443)) {
		t.Fatal("封禁期间访问任何目标都应阻断")
	}
	recs = a.Flush()
	if len(recs) != 1 || recs[0].RuleID != 3 || recs[0].Action != "ban" || recs[0].Network != "udp" {
		t.Fatalf("封禁记录不符: %+v", recs)
	}
	now = now.Add(BanDuration)
	if a.Check(3, "3.3.3.3", "tcp", domain("good.com", 443)) {
		t.Fatal("封禁到期后应放行")
	}

	// 上报失败放回，与新记录合并
	a.Check(1, "1.1.1.1", "tcp", domain("example.com", 80))
	recs = a.Flush()
	a.Check(1, "1.1.1.1", "tcp", domain("example.com", 80))
	a.Restore(recs)
	recs = a.Flush()
	if len(recs) != 1 || recs[0].Count != 2 {
		t.Fatalf("合并不符: %+v", recs)
	}

	// 规则清空
	a.SetRules(nil)
	if a.Check(1, "1.1.1.1", "tcp", domain("bad.com", 443)) {
		t.Fatal("清空规则后不应阻断")
	}
}
