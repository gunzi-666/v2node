package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
	"github.com/wyx2685/v2node/machine"
	"github.com/wyx2685/v2node/node"
)

const (
	// revokeAfter 连续收到这么多次 404 才认定凭据已吊销，避免面板短暂异常导致停机。
	revokeAfter = 3
	// retryInterval 尚未拿到配置或已被吊销时的重试间隔。
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

// logRank 日志详细程度，越小越详细。
func logRank(level string) int {
	switch level {
	case "debug":
		return 0
	case "info", "":
		return 1
	case "warn", "warning":
		return 2
	case "error":
		return 3
	}
	return 4
}

// mostVerbose 取最详细的日志级别。
func mostVerbose(levels []string) string {
	best := ""
	for _, l := range levels {
		if l != "" && (best == "" || logRank(l) < logRank(best)) {
			best = l
		}
	}
	if best == "" {
		return "info"
	}
	return best
}

// panelState 一个已接入的面板。
type panelState struct {
	client *machine.Client
	// cfg 面板下发的配置，尚未获取或凭据已被吊销时为 nil。
	cfg   *machine.Config
	fails int
	next  time.Time
	// tried 已尝试过的更新指令，同一指令只尝试一次。
	tried int64
	// errs 最近一次重建时本面板被跳过的节点，尚未重建过时为 nil。
	errs []machine.NodeError
}

func (p *panelState) interval() time.Duration {
	if p.cfg == nil {
		return retryInterval
	}
	d := time.Duration(p.cfg.PullInterval) * time.Second
	if d <= 0 {
		return defaultPull
	}
	return max(d, minPull)
}

// machineRunner 节点机模式：可接入多个面板，并与 config.json 中的节点一起运行；所有节点共用一个核心，
// 任一来源变化时全部重建。端口冲突或启动失败的节点只跳过并上报所属面板，不自动重试。
type machineRunner struct {
	panels   []*panelState
	confPath string
	// file config.json 的内容，不存在或读取失败时为 nil。
	file     *conf.Conf
	reloadCh chan struct{}
	nodes    *node.Node
	core     *core.V2Core
}

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

// loadFile 读取 config.json，不存在时不报错。
func (r *machineRunner) loadFile() {
	r.file = nil
	if r.confPath == "" {
		return
	}
	if _, err := os.Stat(r.confPath); err != nil {
		return
	}
	c := conf.New()
	if err := c.LoadFromPath(r.confPath); err != nil {
		log.WithField("err", err).Errorf("读取 %s 失败，只运行面板下发的节点", r.confPath)
		return
	}
	r.file = c
}

// item 待启动的节点；owner 为所属面板下标，-1 表示 config.json。
type item struct {
	owner int
	cfg   conf.NodeConfig
}

type portUse struct {
	node.Port
	owner  int
	nodeID int
}

// conflictMsg 端口冲突的原因：占用来源为本面板节点时写明节点 ID，为其它面板时不透露其地址。
func conflictMsg(it item, p node.Port, by portUse) string {
	switch {
	case by.owner == it.owner && it.owner >= 0:
		return fmt.Sprintf("端口 %s 与本面板节点 #%d 冲突，已跳过", p, by.nodeID)
	case by.owner == -1:
		return fmt.Sprintf("端口 %s 已被本机配置文件中的节点占用，已跳过", p)
	}
	return fmt.Sprintf("端口 %s 已被本机其它面板的节点占用，已跳过", p)
}

// apply 按 config.json 与各面板当前配置重建全部节点。
func (r *machineRunner) apply() {
	r.stop()
	base := conf.New()
	if r.file != nil {
		cp := *r.file
		base = &cp
	}
	var items []item
	levels := []string{}
	if r.file != nil {
		levels = append(levels, r.file.LogConfig.Level)
		for _, nc := range r.file.NodeConfigs {
			items = append(items, item{owner: -1, cfg: nc})
		}
	}
	for i, p := range r.panels {
		if p.cfg == nil {
			continue
		}
		levels = append(levels, p.cfg.LogLevel)
		pc := p.cfg.Conf(p.client.Cred)
		if len(pc.NodeConfigs) > 0 {
			if err := os.MkdirAll(machine.CertDir(p.client.Cred.Panel), 0o700); err != nil {
				log.WithField("err", err).Error("创建证书目录失败")
			}
		}
		for _, nc := range pc.NodeConfigs {
			items = append(items, item{owner: i, cfg: nc})
		}
	}
	base.LogConfig.Level = mostVerbose(levels)
	base.NodeConfigs = nil
	setLogLevel(base.LogConfig.Level)

	errsBy := make([][]machine.NodeError, len(r.panels))
	skip := func(it item, reason, detail string) {
		if it.owner < 0 {
			log.Errorf("配置文件中的节点 %s #%d 未启动：%s", it.cfg.APIHost, it.cfg.NodeID, detail)
			return
		}
		log.Errorf("面板 %s 的节点 #%d 未启动：%s", r.panels[it.owner].client.Cred.Panel, it.cfg.NodeID, detail)
		errsBy[it.owner] = append(errsBy[it.owner], machine.NodeError{NodeID: it.cfg.NodeID, Error: reason})
	}

	var prepared []*node.Prepared
	var kept []item
	var used []portUse
	for _, it := range items {
		pr, err := node.Prepare(it.cfg)
		if err != nil {
			msg := "获取节点信息失败: " + err.Error()
			skip(it, msg, msg)
			continue
		}
		var hit *portUse
		var hitPort node.Port
		for _, p := range node.Ports(pr.Info) {
			for i := range used {
				if used[i].Overlap(p) {
					hit, hitPort = &used[i], p
					break
				}
			}
			if hit != nil {
				break
			}
		}
		if hit != nil {
			skip(it, conflictMsg(it, hitPort, *hit), fmt.Sprintf("端口 %s 已被 %s 节点 #%d 占用", hitPort, r.ownerName(hit.owner), hit.nodeID))
			continue
		}
		for _, p := range node.Ports(pr.Info) {
			used = append(used, portUse{Port: p, owner: it.owner, nodeID: it.cfg.NodeID})
		}
		prepared = append(prepared, pr)
		kept = append(kept, it)
	}

	started := 0
	if len(prepared) == 0 {
		log.Warn("没有可运行的节点，等待面板分配")
	} else {
		infos := make([]*panel.NodeInfo, 0, len(prepared))
		for _, pr := range prepared {
			infos = append(infos, pr.Info)
		}
		v2core := core.New(base)
		v2core.ReloadCh = r.reloadCh
		if err := v2core.Start(infos); err != nil {
			log.WithField("err", err).Error("启动核心失败")
			if v2core.Server != nil {
				_ = v2core.Server.Close()
			}
			for _, it := range kept {
				skip(it, "启动核心失败，详见节点机日志", "启动核心失败: "+err.Error())
			}
		} else {
			r.core = v2core
			nodes, errs := node.StartEach(prepared, v2core)
			r.nodes = nodes
			for i, err := range errs {
				if err != nil {
					msg := "启动失败: " + err.Error()
					skip(kept[i], msg, msg)
					continue
				}
				started++
			}
		}
	}
	for i, p := range r.panels {
		p.errs = errsBy[i]
		if p.errs == nil {
			p.errs = []machine.NodeError{}
		}
	}
	log.Infof("已启动 %d 个节点（共 %d 个）", started, len(items))
	r.reportNodes()
}

func (r *machineRunner) ownerName(owner int) string {
	if owner < 0 {
		return "配置文件"
	}
	return "面板 " + r.panels[owner].client.Cred.Panel
}

// reportNodes 向各面板上报其被跳过的节点（列表没变化时不请求）。
func (r *machineRunner) reportNodes() {
	for _, p := range r.panels {
		if p.cfg == nil || p.errs == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := p.client.ReportNodes(ctx, p.errs); err != nil {
			log.WithField("err", err).Warnf("向面板 %s 上报未启动的节点失败，下次拉取时重试", p.client.Cred.Panel)
		}
		cancel()
	}
}

// poll 拉取一个面板的配置，返回是否需要重建节点。
func (r *machineRunner) poll(p *panelState) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, changed, err := p.client.Fetch(ctx)
	switch {
	case errors.Is(err, machine.ErrRevoked):
		p.fails++
		if p.fails >= revokeAfter && p.cfg != nil {
			log.Errorf("面板 %s 拒绝了本机凭据（已吊销、已删除或面板关闭了节点机），停止该面板的节点；重新接入请在面板生成新的安装命令", p.client.Cred.Panel)
			p.cfg, p.errs = nil, nil
			p.client.Reset()
			return true
		}
		if p.fails < revokeAfter {
			log.WithField("次数", p.fails).Warnf("面板 %s 拒绝了本机凭据", p.client.Cred.Panel)
		}
		return false
	case err != nil:
		log.WithField("err", err).Warnf("获取面板 %s 的节点机配置失败，保持当前状态", p.client.Cred.Panel)
		return false
	}
	p.fails = 0
	if !changed {
		return false
	}
	rebuild := !p.cfg.SameNodes(cfg)
	if rebuild {
		log.Infof("面板 %s 的节点机配置已更新，重新加载", p.client.Cred.Panel)
	}
	p.cfg = cfg
	return rebuild
}

// checkUpdate 按面板指令更新程序：下载校验替换后停止节点并原地重启，失败时保留当前程序并上报原因。
// 接入多个面板时只接受比当前版本新的版本，防止两个面板来回降级。
func (r *machineRunner) checkUpdate() {
	for _, p := range r.panels {
		if p.cfg == nil || p.cfg.Update == nil {
			continue
		}
		u := p.cfg.Update
		if u.Version == version || u.ID == p.tried {
			continue
		}
		p.tried = u.ID
		if len(r.panels) > 1 && !machine.Newer(u.Version, version) {
			log.Infof("面板 %s 要求更新到 %s，不比当前版本新，已忽略（接入多个面板时只升级不降级）", p.client.Cred.Panel, u.Version)
			continue
		}
		log.Infof("面板 %s 要求更新到 %s，开始下载", p.client.Cred.Panel, u.Version)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		exe, err := (&machine.Updater{BaseURL: os.Getenv("V2NODE_RELEASE_BASE")}).Install(ctx, u.Version)
		cancel()
		if err != nil {
			log.WithField("err", err).Error("更新失败，继续运行当前版本")
			p.client.Report(u.ID, err)
			continue
		}
		log.Infof("已更新到 %s，重启程序", u.Version)
		r.stop()
		err = machine.Restart(exe)
		log.WithField("err", err).Error("重启失败，新版本将在下次启动时生效")
		p.client.Report(u.ID, err)
		r.apply()
		return
	}
}

// startExtras 按 config.json 设置日志输出与 pprof（只在启动时生效一次）。
func startExtras(c *conf.Conf) {
	if c == nil {
		return
	}
	if c.LogConfig.Output != "" {
		f, err := os.OpenFile(c.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		} else {
			log.SetOutput(f)
		}
	}
	if c.PprofPort != 0 {
		go func() {
			log.Infof("Starting pprof server on :%d", c.PprofPort)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", c.PprofPort), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}
}

func machineHandle(creds []machine.Stored, confPath string, watchFile bool) {
	r := &machineRunner{confPath: confPath, reloadCh: make(chan struct{}, 1)}
	for _, s := range creds {
		log.Infof("节点机模式：面板 %s，节点机 #%d", s.Cred.Panel, s.Cred.MachineID)
		r.panels = append(r.panels, &panelState{client: &machine.Client{Cred: s.Cred, Version: version}})
	}
	r.loadFile()
	if r.file != nil {
		log.Infof("同时运行 %s 中的 %d 个节点", confPath, len(r.file.NodeConfigs))
	}
	startExtras(r.file)
	limiter.Init()
	defer r.stop()

	fileCh := make(chan struct{}, 1)
	if watchFile && r.file != nil {
		// Watch 会改写传入的对象，这里只用它发信号，收到后重新读取文件。
		if err := conf.New().Watch(confPath, func() {
			select {
			case fileCh <- struct{}{}:
			default:
			}
		}); err != nil {
			log.WithField("err", err).Error("监听配置文件失败")
		}
	}

	// 启动时先拉取全部面板，再统一启动一次。
	now := time.Now()
	for _, p := range r.panels {
		r.poll(p)
		p.next = now.Add(p.interval())
	}
	r.apply()
	r.checkUpdate()

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	timer := time.NewTimer(r.wait())
	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			r.stop()
			os.Exit(0)
		case <-r.reloadCh:
			log.Info("收到重启信号，正在重新加载...")
			r.apply()
		case <-fileCh:
			log.Infof("%s 已变化，重新加载", confPath)
			r.loadFile()
			r.apply()
		case <-timer.C:
			now := time.Now()
			rebuild := false
			for _, p := range r.panels {
				if now.Before(p.next) {
					continue
				}
				rebuild = r.poll(p) || rebuild
				p.next = time.Now().Add(p.interval())
			}
			if rebuild {
				r.apply()
			} else {
				r.reportNodes()
			}
			r.checkUpdate()
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(r.wait())
	}
}

// wait 距离最早一个面板下次拉取的时间。
func (r *machineRunner) wait() time.Duration {
	next := time.Time{}
	for _, p := range r.panels {
		if next.IsZero() || p.next.Before(next) {
			next = p.next
		}
	}
	return max(time.Until(next), time.Second)
}
