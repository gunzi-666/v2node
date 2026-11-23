package cmd

import (
	"strings"
	"testing"

	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/node"
)

func TestMostVerbose(t *testing.T) {
	for want, levels := range map[string][]string{
		"info":    nil,
		"debug":   {"error", "debug", "info"},
		"warning": {"none", "warning", ""},
		"error":   {"none", "error"},
		"none":    {"none"},
	} {
		if got := mostVerbose(levels); got != want {
			t.Errorf("mostVerbose(%v) = %q，应为 %q", levels, got, want)
		}
	}
}

func TestConflictMsg(t *testing.T) {
	p := node.Port{Port: 443}
	it := item{owner: 1, cfg: conf.NodeConfig{APIHost: "https://b.com", NodeID: 5}}
	if got := conflictMsg(it, p, portUse{owner: 1, nodeID: 3}); got != "端口 443/TCP 与本面板节点 #3 冲突，已跳过" {
		t.Errorf("同面板: %q", got)
	}
	if got := conflictMsg(it, p, portUse{owner: -1, nodeID: 3}); got != "端口 443/TCP 已被本机配置文件中的节点占用，已跳过" {
		t.Errorf("配置文件: %q", got)
	}
	got := conflictMsg(it, p, portUse{owner: 0, nodeID: 3})
	if got != "端口 443/TCP 已被本机其它面板的节点占用，已跳过" || strings.Contains(got, "#3") {
		t.Errorf("其它面板不应透露其节点或地址: %q", got)
	}
}
