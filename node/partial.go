package node

import (
	"context"
	"errors"
	"strconv"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

// 节点机模式（可同时接入多个面板）下逐个准备、启动节点：某个节点失败时只跳过它，其余节点照常运行。

// Prepared 已获取节点信息、尚未启动的节点。
type Prepared struct {
	Config conf.NodeConfig
	Client *panel.Client
	Info   *panel.NodeInfo
}

// Prepare 向面板获取节点信息。
func Prepare(c conf.NodeConfig) (*Prepared, error) {
	p, err := panel.New(&c)
	if err != nil {
		return nil, err
	}
	info, err := p.GetNodeInfo(context.Background())
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, errors.New("面板未返回节点信息")
	}
	return &Prepared{Config: c, Client: p, Info: info}, nil
}

// StartEach 逐个启动节点，失败的节点关闭后跳过；errs 与 items 一一对应，成功为 nil。
func StartEach(items []*Prepared, x *core.V2Core) (n *Node, errs []error) {
	n = &Node{}
	errs = make([]error, len(items))
	for i, it := range items {
		cfg := it.Config
		c := NewController(it.Client, &cfg, it.Info)
		if err := c.Start(x); err != nil {
			_ = c.Close()
			errs[i] = err
			continue
		}
		n.controllers = append(n.controllers, c)
		n.NodeInfos = append(n.NodeInfos, it.Info)
	}
	return n, errs
}

// Port 节点占用的监听端口。
type Port struct {
	IP   string
	Port int
	UDP  bool
}

func (p Port) String() string {
	if p.UDP {
		return strconv.Itoa(p.Port) + "/UDP"
	}
	return strconv.Itoa(p.Port) + "/TCP"
}

// Ports 节点占用的端口：Hysteria2、TUIC 与 mKCP / QUIC 传输为 UDP，Shadowsocks 同时占用 TCP 与 UDP，其它为 TCP。
func Ports(info *panel.NodeInfo) []Port {
	if info == nil || info.Common == nil || info.Common.ServerPort <= 0 {
		return nil
	}
	cm := info.Common
	tcp := Port{IP: cm.ListenIP, Port: cm.ServerPort}
	udp := Port{IP: cm.ListenIP, Port: cm.ServerPort, UDP: true}
	switch info.Type {
	case "hysteria2", "tuic":
		return []Port{udp}
	case "shadowsocks":
		return []Port{tcp, udp}
	}
	if cm.Network == "kcp" || cm.Network == "quic" {
		return []Port{udp}
	}
	return []Port{tcp}
}

func anyIP(ip string) bool { return ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]" }

// Overlap 两个端口是否冲突：端口与传输层相同，且监听地址有一方为全部地址或两者相同。
func (p Port) Overlap(o Port) bool {
	return p.Port == o.Port && p.UDP == o.UDP && (anyIP(p.IP) || anyIP(o.IP) || p.IP == o.IP)
}
