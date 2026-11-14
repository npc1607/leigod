# Throne (Sing-box) 联动雷神隔离命名空间配置指南

本项目已经在 `leigod-ns` 独立网络命名空间内内置了一个以非 root 身份 (`uid=65534`) 运行的 **RFC 1928 全功能 SOCKS5 服务端（同时支持 TCP `CONNECT` 和游戏所需的 `UDP ASSOCIATE`）**，并在宿主机暴露了监听地址：

- 宿主机回环入口：`127.0.0.1:15588`
- 命名空间直连入口：`10.254.254.2:1080`（推荐在 Throne TUN 模式下填写此地址，TCP 与 UDP 均直达 `leigod-ns`）

---

## 一、为什么不会和 Throne 的 TUN 模式冲突？

1. **去程与回程物理网卡强绑（杜绝二次代理）**：
   - `leigod` 会在宿主机创建高优先级策略路由规则（`pref 89/90 -> table 2540`）：
     - `from 10.254.254.0/24 lookup 2540`
     - `to 10.254.254.0/24 lookup 2540`
   - 凡是来自或发往 `leigod-ns` (`10.254.254.2`) 的流量，直接通过物理网卡（如 `enp34s0`）发往路由器网关，**完全绕过 Throne 的 `nekobox-tun` / `tun0`**，绝不会发生“雷神节点流量被 Throne 代理”的套娃冲突。
2. **宿主机防火墙零污染**：
   - 雷神闭源程序 `acc-gw.router.amd64` 创建的 `tun` 网卡、`ipset`、`iptables -t mangle OUTPUT` 规则 100% 被关在 `leigod-ns` 内部。

---

## 二、两种游戏加速使用方法（任选其一，也可混用）

### 方法 A：Steam 启动选项按进程直接进入 `leigod-ns`（最简单、零代理损耗）

无需修改 Throne 的任何配置！直接在 Steam 中右键需要加速的游戏 -> **属性** -> **启动选项**，填入：

```sh
leigod run -- %command%
```

- **原理**：`leigod run` 通过两阶段 Capability 跳板（`leigod-nsenter`）将游戏进程直接放入 `leigod-ns` 网络命名空间，同时 **100% 保留普通桌面用户权限（`uid=1000`）以及 Steam Overlay (`LD_PRELOAD`)、Proton (`LD_LIBRARY_PATH`)、PipeWire 音频、Wayland/X11 等全部环境变量**。
- 游戏进程在 `leigod-ns` 内发出的所有 TCP / UDP / ICMP 流量直接进入雷神的 `tun` 虚拟网卡加速。

---

### 方法 B：在 Throne 中配置按进程分流到雷神 SOCKS5（适合非 Steam 启动器或全局统一管理）

如果你希望直接双击游戏启动，由 Throne 自动把该游戏的流量转交给雷神加速：

#### 步骤 1：在 Throne 中新建一个 SOCKS5 节点
1. 打开 Throne 主界面，在配置分组中点击 **新建配置 (New Profile)**：
   - **类型 (Type)**：`SOCKS` (SOCKS5)
   - **名称 (Name)**：`Leigod-NS`
   - **服务器地址 (Address)**：`10.254.254.2`
   - **端口 (Port)**：`1080`（或填 `127.0.0.1` 端口 `15588`）
   - 认证留空，点击保存。

#### 步骤 2：在 Throne 路由设置中添加游戏进程分流规则
1. 打开 Throne **设置 (Settings)** -> **路由设置 (Routing Settings)** -> **自定义路由规则 (Custom Route Rules)**。
2. 新增一条规则（放在顶部高优先级位置）：
   - **匹配字段 `process_name`**（进程名）：填写游戏的进程名（例如 `cs2`、`r5apex.exe`、`bf2042.exe`、`eldenring.exe`、`overwatch.exe`）
   - **出站 (Outbound)**：选择刚刚创建的 `Leigod-NS` 节点。

等价的 Sing-box 路由规则 JSON 片段参考：

```json
{
  "outbounds": [
    {
      "type": "socks",
      "tag": "leigod-out",
      "server": "10.254.254.2",
      "server_port": 1080,
      "version": "5"
    }
  ],
  "route": {
    "rules": [
      {
        "process_name": [
          "cs2",
          "r5apex.exe",
          "PUBG-Win64-Shipping.exe",
          "PathOfExileSteam.exe"
        ],
        "outbound": "leigod-out"
      }
    ]
  }
}
```
