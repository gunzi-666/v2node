# v2node
A v2board backend base on moddified xray-core.
一个基于修改版xray内核的V2board节点服务端。

**注意： 本项目需要搭配[修改版V2board](https://github.com/wyx2685/v2board)**

## 软件安装

### 一键安装

```
wget -N https://raw.githubusercontent.com/gunzi-666/v2node/main/script/install.sh && bash install.sh
```

### 一键安装（Gvboard 版，含审计）

```
wget -N https://raw.githubusercontent.com/gunzi-666/v2node/gvboard/script/install.sh && bash install.sh
```

Gvboard 版从 `gvboard` 分支发布，版本号带 `-gvboard` 后缀（如 `v0.4.9-gvboard.1`），以预发布版本发布，不会成为常规版本的“最新版”。安装、`v2node update` 只会安装 Gvboard 版；指定版本时请写完整版本号。

## 节点机模式（Gvboard）

Gvboard 面板在系统配置“节点”中开启“节点机一次性接入”后，在“服务器 → 节点机”添加节点机，复制面板生成的命令在服务器上以 root 执行即可：

```
wget -N https://raw.githubusercontent.com/gunzi-666/v2node/gvboard/script/install.sh && bash install.sh --panel https://panel.example.com --code 面板生成的接入码
```

- 接入码 30 分钟内有效、只能使用一次；接入后本机凭据保存在 `/etc/v2node/machine.json`（仅 root 可读），不需要通讯密钥，也不需要编写配置文件。已安装的 v2node 也可以直接执行 `v2node enroll --panel 面板地址 --code 接入码` 后重启。
- 运行哪些节点、各节点是否开启审计、日志级别都由面板下发，v2node 按面板的拉取间隔获取，变化后自动重新加载。
- 面板吊销或删除节点机后（连续 3 次被面板拒绝），v2node 停止全部节点，之后继续定期重试；重新接入请在面板生成新的安装命令。
- 远程更新（`v0.4.9-gvboard.3` 起）：面板「节点机」页面下发版本号后，v2node 从本仓库的 GitHub Release 下载本机架构的安装包，按 `.dgst` 校验 sha256、确认新程序能运行后替换并原地重启；失败时保留原程序并把原因报给面板。只更新程序，不更新 geoip / geosite；支持 Linux 与 FreeBSD。
- 存在 `/etc/v2node/machine.json` 时忽略 `config.json`；删除该文件并重启即回到配置文件方式。原版 v2board 请继续使用配置文件方式。

## 审计（Gvboard）

搭配 [Gvboard](https://github.com/gunzi-666/gvboard) 面板时，可以在节点配置中开启审计：

```json
{
    "Nodes": [
        {
            "ApiHost": "https://panel.example.com",
            "NodeID": 1,
            "ApiKey": "节点通信密钥",
            "AuditEnable": true
        }
    ]
}
```

- `AuditEnable` 默认关闭，关闭时与常规版本完全相同，不请求审计接口。
- 开启后随节点信息一起从面板拉取审计规则（`/api/v1/server/UniProxy/audit`），随流量一起上报命中记录。
- 规则动作：记录（放行并上报）、阻断（断开连接并上报）、封禁用户（断开连接，本节点 10 分钟内拒绝该用户的所有连接，面板收到上报后封禁整个账号）。
- 节点不做 DNS 解析：域名规则只匹配客户端请求或嗅探到的域名，IP 规则只匹配直接以 IP 访问的连接。
- 面板不支持审计接口（返回 404）时视为没有规则。

## 构建
``` bash
GOEXPERIMENT=jsonv2 go build -v -o build_assets/v2node -trimpath -ldflags "-X 'github.com/wyx2685/v2node/cmd.version=$version' -s -w -buildid="
```

## Stars 增长记录

[![Stargazers over time](https://starchart.cc/wyx2685/v2node.svg?variant=adaptive)](https://starchart.cc/wyx2685/v2node)
