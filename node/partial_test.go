package node

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func info(typ, network, ip string, port int) *panel.NodeInfo {
	return &panel.NodeInfo{Type: typ, Common: &panel.CommonNode{Protocol: typ, Network: network, ListenIP: ip, ServerPort: port}}
}

func overlap(a, b *panel.NodeInfo) bool {
	for _, p := range Ports(a) {
		for _, q := range Ports(b) {
			if p.Overlap(q) {
				return true
			}
		}
	}
	return false
}

func TestPorts(t *testing.T) {
	for name, c := range map[string]struct {
		a, b *panel.NodeInfo
		want bool
	}{
		"同端口 TCP":           {info("vless", "tcp", "", 443), info("trojan", "ws", "0.0.0.0", 443), true},
		"TCP 与 UDP 不冲突":     {info("vless", "tcp", "", 443), info("hysteria2", "", "", 443), false},
		"两个 UDP 冲突":         {info("tuic", "", "", 443), info("hysteria2", "", "", 443), true},
		"Shadowsocks 占 UDP": {info("shadowsocks", "", "", 8388), info("hysteria2", "", "", 8388), true},
		"mKCP 为 UDP":        {info("vmess", "kcp", "", 1000), info("vless", "tcp", "", 1000), false},
		"不同端口":              {info("vless", "tcp", "", 443), info("vless", "tcp", "", 444), false},
		"不同监听地址":            {info("vless", "tcp", "1.1.1.1", 443), info("vless", "tcp", "2.2.2.2", 443), false},
		"相同监听地址":            {info("vless", "tcp", "1.1.1.1", 443), info("vless", "tcp", "1.1.1.1", 443), true},
		"全部地址与具体地址":         {info("vless", "tcp", "::", 443), info("vless", "tcp", "1.1.1.1", 443), true},
	} {
		if got := overlap(c.a, c.b); got != c.want {
			t.Errorf("%s: 冲突 = %v，应为 %v", name, got, c.want)
		}
	}
	if got := Ports(info("shadowsocks", "", "", 8388)); len(got) != 2 || got[0].String() != "8388/TCP" || got[1].String() != "8388/UDP" {
		t.Fatalf("Shadowsocks 端口不符: %v", got)
	}
	if Ports(&panel.NodeInfo{}) != nil {
		t.Fatal("没有节点参数时不占用端口")
	}
}
