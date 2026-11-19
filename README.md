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
