// Package audit 审计规则（Gvboard）：按面板下发的规则记录、阻断或封禁用户对指定目标的访问。
// 匹配语法与 Gvboard 面板一致。
package audit

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

const (
	kindDomain  = "domain"  // 域名及其子域名
	kindFull    = "full"    // 完整域名
	kindKeyword = "keyword" // 域名包含
	kindRegexp  = "regexp"  // 域名正则
	kindIP      = "ip"      // IP 或 IP 段
	kindPort    = "port"    // 只匹配端口
)

// Target 一次访问的目标。Domain 为空表示不知道域名，IP 无效表示不知道 IP，Port 为 0 表示不知道端口。
type Target struct {
	Domain string
	IP     netip.Addr
	Port   uint16
}

// String 记录用的目标写法：host:port，IPv6 加方括号。
func (t Target) String() string {
	host := t.Domain
	if host == "" && t.IP.IsValid() {
		host = t.IP.String()
		if t.IP.Is6() && t.Port > 0 {
			host = "[" + host + "]"
		}
	}
	if t.Port == 0 {
		return host
	}
	return host + ":" + strconv.Itoa(int(t.Port))
}

// entry 一条匹配内容。portLo 为 0 表示不限端口。
type entry struct {
	kind   string
	value  string
	re     *regexp.Regexp
	prefix netip.Prefix
	portLo uint16
	portHi uint16
}

func (e *entry) match(t Target) bool {
	if e.portLo > 0 && (t.Port < e.portLo || t.Port > e.portHi) {
		return false
	}
	switch e.kind {
	case kindPort:
		return true
	case kindIP:
		return t.IP.IsValid() && e.prefix.Contains(t.IP.Unmap())
	}
	if t.Domain == "" {
		return false
	}
	switch e.kind {
	case kindDomain:
		return t.Domain == e.value || strings.HasSuffix(t.Domain, "."+e.value)
	case kindFull:
		return t.Domain == e.value
	case kindKeyword:
		return strings.Contains(t.Domain, e.value)
	case kindRegexp:
		return e.re.MatchString(t.Domain)
	}
	return false
}

var domainChars = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*\.?$`)

// parseLine 解析一行匹配内容。
func parseLine(l string) (*entry, error) {
	l = strings.TrimSpace(l)
	if len(l) >= 7 && strings.EqualFold(l[:7], "regexp:") {
		re, err := regexp.Compile("(?i)" + l[7:])
		if l[7:] == "" || err != nil {
			return nil, fmt.Errorf("invalid regexp")
		}
		return &entry{kind: kindRegexp, re: re}, nil
	}
	l = strings.ToLower(l)
	if v, ok := strings.CutPrefix(l, "keyword:"); ok {
		if v == "" {
			return nil, fmt.Errorf("empty keyword")
		}
		return &entry{kind: kindKeyword, value: v}, nil
	}
	kind := ""
	if v, ok := strings.CutPrefix(l, "full:"); ok {
		kind, l = kindFull, v
	} else if v, ok := strings.CutPrefix(l, "domain:"); ok {
		kind, l = kindDomain, v
	}
	host, port, err := splitHostPort(l)
	if err != nil {
		return nil, err
	}
	e := &entry{}
	if port != "" {
		if e.portLo, e.portHi, err = parsePorts(port); err != nil {
			return nil, err
		}
	}
	if host == "" {
		if kind != "" || port == "" {
			return nil, fmt.Errorf("invalid format")
		}
		e.kind = kindPort
		return e, nil
	}
	if kind == "" {
		if p, ok := parseIPPrefix(host); ok {
			e.kind, e.prefix = kindIP, p
			return e, nil
		}
		kind = kindDomain
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "*."), ".")
	if host == "" || len(host) > 253 || !domainChars.MatchString(host) {
		return nil, fmt.Errorf("invalid domain")
	}
	e.kind, e.value = kind, host
	return e, nil
}

// splitHostPort 拆分主机与端口：[IPv6]:端口、主机:端口、不带端口的 IPv6（含多个冒号）。
func splitHostPort(s string) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", fmt.Errorf("missing ]")
		}
		host, rest := s[1:end], s[end+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") || len(rest) == 1 {
			return "", "", fmt.Errorf("invalid format")
		}
		return host, rest[1:], nil
	}
	switch strings.Count(s, ":") {
	case 0:
		return s, "", nil
	case 1:
		host, port, _ = strings.Cut(s, ":")
		if port == "" {
			return "", "", fmt.Errorf("missing port")
		}
		return host, port, nil
	}
	return s, "", nil
}

// parsePorts 解析端口或端口段（6881-6889）。
func parsePorts(s string) (uint16, uint16, error) {
	lo, hi, isRange := strings.Cut(s, "-")
	a, err1 := strconv.ParseUint(lo, 10, 16)
	b := a
	var err2 error
	if isRange {
		b, err2 = strconv.ParseUint(hi, 10, 16)
	}
	if err1 != nil || err2 != nil || a == 0 || b == 0 || a > b {
		return 0, 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(a), uint16(b), nil
}

func parseIPPrefix(s string) (netip.Prefix, bool) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return p, false
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p.Masked(), p.Bits() >= 0
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, false
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), true
}
