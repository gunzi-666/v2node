package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
	"github.com/wyx2685/v2node/machine"
	"github.com/wyx2685/v2node/node"
)

const (
	// revokeAfter 连续收到这么多次 404 才认定凭据已吊销，避免面板短暂异常导致停机。
	revokeAfter = 3
	// retryInterval 尚未拿到配置或已停止时的重试间隔。
	retryInterval = 10 * time.Second
	minPull       = 10 * time.Second
	defaultPull   = 60 * time.Second
)

func setLogLevel(level string) {
	switch level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error", "none":
		log.SetLevel(log.ErrorLevel)
	}
}

// machineRunner 节点机模式：按面板下发的配置启动节点，配置变化时重建，凭据被吊销时停止全部节点。
type machineRunner struct {
	client   *machine.Client
	reloadCh chan struct{}
	nodes    *node.Node
	core     *core.V2Core
	cfg      *machine.Config
	fails    int
	// started 当前配置已成功启动（未分配节点也算），节点没变化时不重建。
	started bool
	// triedUpdate 已尝试过的更新指令，同一指令只尝试一次。
	triedUpdate int64
}

func (r *machineRunner) running() bool { return r.core != nil }

func (r *machineRunner) stop() {
	if r.nodes != nil {
		if err := r.nodes.Close(); err != nil {
			log.WithField("err", err).Error("关闭节点失败")
		}
		r.nodes = nil
	}
	if r.core != nil {
		if err := r.core.Close(); err != nil {
			log.WithField("err", err).Error("关闭核心失败")
		}
		r.core = nil
	}
	runtime.GC()
}

func (r *machineRunner) start(c *conf.Conf) error {
	setLogLevel(c.LogConfig.Level)
	if len(c.NodeConfigs) == 0 {
		log.Warn("面板没有为这台节点机分配节点，等待分配")
		return nil
	}
	nodes, err := node.New(c.NodeConfigs)
	if err != nil {
		return err
	}
	v2core := core.New(c)
	v2core.ReloadCh = r.reloadCh
	if err := v2core.Start(nodes.NodeInfos); err != nil {
		return err
	}
	r.core = v2core
	if err := nodes.Start(c.NodeConfigs, v2core); err != nil {
		return err
	}
	r.nodes = nodes
	return nil
}

// apply 按配置重建节点，失败时停止并在下次拉取时强制重试。
func (r *machineRunner) apply(cfg *machine.Config) {
	r.stop()
	r.cfg, r.started = cfg, false
	if err := r.start(cfg.Conf(r.client.Cred)); err != nil {
		log.WithField("err", err).Error("启动节点失败，稍后重试")
		r.stop()
		r.client.Reset()
		return
	}
	r.started = true
	if r.running() {
		log.Infof("已按面板配置启动 %d 个节点", len(cfg.Nodes))
	}
}

// poll 拉取一次配置，返回下次拉取的间隔。
func (r *machineRunner) poll() time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, changed, err := r.client.Fetch(ctx)
	switch {
	case errors.Is(err, machine.ErrRevoked):
		r.fails++
		if r.fails >= revokeAfter && (r.running() || r.cfg != nil) {
			log.Error("面板拒绝了本机凭据（已吊销、已删除或面板关闭了节点机），停止全部节点；重新接入请在面板生成新的安装命令")
			r.stop()
			r.cfg = nil
			r.client.Reset()
		} else if r.fails < revokeAfter {
			log.WithField("次数", r.fails).Warn("面板拒绝了本机凭据")
		}
		return r.interval()
	case err != nil:
		log.WithField("err", err).Warn("获取节点机配置失败，保持当前状态")
		return r.interval()
	}
	r.fails = 0
	switch {
	case changed && r.started && r.cfg.SameNodes(cfg):
		r.cfg = cfg
	case changed:
		log.Info("节点机配置已更新，重新加载")
		r.apply(cfg)
	}
	r.update(cfg.Update)
	return r.interval()
}

// update 按面板指令更新程序：下载校验替换后停止节点并原地重启，失败时保留当前程序并上报原因。
func (r *machineRunner) update(u *machine.Update) {
	if u == nil || u.Version == version || u.ID == r.triedUpdate {
		return
	}
	r.triedUpdate = u.ID
	log.Infof("面板要求更新到 %s，开始下载", u.Version)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	exe, err := (&machine.Updater{BaseURL: os.Getenv("V2NODE_RELEASE_BASE")}).Install(ctx, u.Version)
	if err != nil {
		log.WithField("err", err).Error("更新失败，继续运行当前版本")
		r.client.Report(u.ID, err)
		return
	}
	log.Infof("已更新到 %s，重启程序", u.Version)
	r.stop()
	err = machine.Restart(exe)
	log.WithField("err", err).Error("重启失败，新版本将在下次启动时生效")
	r.client.Report(u.ID, err)
	if r.cfg != nil {
		r.apply(r.cfg)
	}
}

func (r *machineRunner) interval() time.Duration {
	if r.cfg == nil {
		return retryInterval
	}
	d := time.Duration(r.cfg.PullInterval) * time.Second
	if d <= 0 {
		return defaultPull
	}
	return max(d, minPull)
}

func machineHandle(path string) {
	cred, err := machine.Load(path)
	if err != nil {
		log.WithField("err", err).Error("读取节点机凭据失败")
		return
	}
	log.Infof("节点机模式：面板 %s，节点机 #%d", cred.Panel, cred.MachineID)
	limiter.Init()
	r := &machineRunner{client: &machine.Client{Cred: cred, Version: version}, reloadCh: make(chan struct{}, 1)}
	defer r.stop()

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	timer := time.NewTimer(0)
	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			r.stop()
			os.Exit(0)
		case <-r.reloadCh:
			if r.cfg != nil {
				log.Info("收到重启信号，正在重新加载...")
				r.apply(r.cfg)
			}
		case <-timer.C:
			timer.Reset(r.poll())
		}
	}
}
