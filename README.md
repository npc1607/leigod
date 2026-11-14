# leigod — Linux 雷神加速器隔离命名空间方案 (Network Namespace + Throne/Sing-box 联动)

将雷神加速器官方 Linux / SteamDeck 插件（`acc-gw.router.amd64`）运行在**完全独立的 Linux Network Namespace (`leigod-ns`) 与私有 Mount Namespace** 中，实现：

1. **宿主机零污染**：雷神创建的所有 `tun` 设备、`ipset`、`iptables -t mangle OUTPUT` 劫持规则、`ip rule` 策略路由、以及 `/etc/os-release` / `product_name` / `/home/leigod` 伪装文件**全部被关在隔离命名空间内**，宿主机网络栈与文件系统保持 100% 干净。
2. **与 Throne (Sing-box) / Clash / dae 完美共存**：
   - 宿主机侧通过高优先级策略路由表（`table 2540`, `pref 89/90`）将 `leigod-ns` 的进出流量直接绑定到物理有线网卡（如 `enp34s0`），**100% 绕过宿主机 Throne 的 TUN 虚拟网卡**，彻底杜绝“雷神加速流量被科学上网二次代理”。
3. **内置两大加速接入模式（方案 2 + 方案 3 合一）**：
   - **模式一（方案 2 - 按进程隔离运行）**：在 Steam 启动选项填入 `leigod run -- %command%`，免 `sudo` 密码、不丢失 `LD_PRELOAD`（Steam Overlay 正常），仅让该游戏进入 `leigod-ns` 享受原生 TUN 加速。
   - **模式二（方案 3 - Throne / Sing-box 联动分流）**：内置运行在 `leigod-ns` 内的非 root **全功能 SOCKS5 服务端（支持 TCP `CONNECT` + 游戏 `UDP ASSOCIATE`）**，可在 Throne 中直接按进程名 (`process_name`) 将游戏流量分流至 `10.254.254.2:1080` / `127.0.0.1:15588`。
4. **零外部琐碎依赖**：内置 WebSocket 5588 门前守卫（替代 `sslh`，防止裸 HTTP 扫描导致官方二进制崩溃）和 UDP 6066 局域网广播发现中继，手机雷神 App 在同一局域网下可直接发现并绑定。

---

## 架构图

```text
┌────────────────────────────────────────────────────────────────────────────┐
│ 宿主机 (Arch Linux, 192.168.1.3, 开启 Throne / Sing-box TUN)                 │
│                                                                            │
│  • 日常流量 (浏览器/Discord) ──> 走宿主机 Throne (Sing-box) 正常分流          │
│  • 宿主机 Bridge 中继:                                                      │
│     ├─ UDP 0.0.0.0:6066 ──> 10.254.254.2:6066 (手机雷神 App 局域网广播发现)   │
│     ├─ TCP 0.0.0.0:5588 ──> 10.254.254.2:5588 (内置 WebSocket 守卫过滤)      │
│     └─ TCP 127.0.0.1:15588 ──> 10.254.254.2:1080 (SOCKS5 本地回环入口)       │
│                                                                            │
│  [leigod-host: 10.254.254.1/24] ──(ip rule pref 89/90 table 2540)──> enp34s0│
└──────┼─────────────────────────────────────────────────────────────────────┘
       │ veth pair (物理网卡直通，绕过宿主机 Throne TUN)
┌──────┼─────────────────────────────────────────────────────────────────────┐
│ 隔离网络命名空间 + 私有挂载命名空间 (leigod-ns)                              │
│  [eth0: 10.254.254.2/24, dummy wlan0: 持久化 MAC + 192.168.1.3/32]          │
│                                                                            │
│  • acc-gw.router.amd64 (root):                                             │
│     在 leigod-ns 内创建 tun 隧道与 iptables mangle OUTPUT 规则              │
│     (自动拦截 leigod-ns 内所有非 root 用户的 TCP/UDP 流量进入雷神加速隧道)   │
│                                                                            │
│  • 接入方式 1: `leigod run -- %command%` (以桌面用户 uid=1000 在 ns 内运行) │
│  • 接入方式 2: 内置 SOCKS5 Server (10.254.254.2:1080, uid=65534 nobody)     │
└────────────────────────────────────────────────────────────────────────────┘
```

---

## 快速开始

### 1. 编译与安装

```sh
cd ~/project/leigod
make build
sudo ./bin/leigod install
```

`install` 命令会自动完成：
- 检查并安装所需依赖（`ipset`、`iptables`、`iproute2`）；
- 下载并校验雷神官方 `acc-gw.router.amd64` 与 `ipdatacloud_country.xdb`；
- 生成基于 `/etc/machine-id` 的持久设备身份 MAC（保存在 `/var/lib/leigod/device-mac`，重装不丢绑定）；
- 安装 `/usr/local/bin/leigod` 与带 `cap_sys_admin+ep` 的 `/opt/leigod/bin/leigod-nsenter` 辅助器；
- 注册并启动 `leigod-isolated.service` systemd 服务。

### 2. 手机雷神 App 绑定与开启加速

1. 确保手机与台式机连接在同一个局域网路由器下；
2. 打开手机 **雷神加速器 App** -> **硬件加速** -> 选择 **SteamDeck** -> 按照页面提示扫描并完成绑定；
3. 在手机 App 上选择你要玩的游戏，点击 **开始加速**。

### 3. 查看运行与加速状态

```sh
leigod status
```

当手机 App 开启游戏加速后，`Game Acceleration TUN` 会显示 `ACTIVE (tun... inside leigod-ns)`。

### 4. 加速你的游戏

- **方式 A（Steam 启动项，最推荐）**：
  在 Steam 游戏属性 -> 启动选项中填写：
  ```sh
  leigod run -- %command%
  ```
  或者在终端直接测试隔离网络连通性：
  ```sh
  leigod run -- curl -4 ipinfo.io
  ```
- **方式 B（配合 Throne 按进程分流）**：
  在 Throne 中添加 SOCKS5 出站 `10.254.254.2:1080`（或 `127.0.0.1:15588`），详见 [`configs/throne-singbox-guide.md`](configs/throne-singbox-guide.md)。

---

## 常用命令

```sh
sudo leigod install     # 安装并启动服务
sudo leigod uninstall   # 停止服务、清理命名空间和路由规则并卸载
sudo leigod start       # 启动服务
sudo leigod stop        # 停止服务并自动清理隔离命名空间
sudo leigod restart     # 重启服务
leigod status           # 查看服务、绑定状态、物理网卡绕行路由和加速 TUN 状态
leigod run -- <cmd>     # 在雷神隔离命名空间内运行指定命令或游戏
```

---

## 状态面板 (Web)

```sh
leigod web                       # http://127.0.0.1:7788
leigod web --listen 0.0.0.0:7788 # 局域网可访问（手机也能看）
```

只读面板，每 1.5s 刷新，展示：隧道延迟曲线、丢包、加速节点、加速/直连命中数、
设备身份与绑定状态、物理网卡绕代理路由、Steam API 初始化结果、游戏进程是否在
隔离命名空间内，以及最近连接（域名 / 目标 / 走的节点）。

普通用户即可运行；以 root 运行时能额外看到命名空间内网卡名等细节。

## 辅助脚本

```sh
sudo ./scripts/reinstall.sh    # 编译 + 保留绑定 + 重装 + 重启
sudo ./scripts/uninstall.sh    # 卸载并清理网络（--purge 彻底清除）
sudo ./scripts/verify.sh       # 全链路 10 组检查，输出 PASS/WARN/FAIL 汇总
```

## 免责声明

本仓库**不包含**雷神官方二进制文件（`acc-gw.router.amd64`、`ipdatacloud_country.xdb`），
安装时从上游下载并做 SHA-256 白名单校验。本项目未对雷神程序做逆向、修改或破解，
仅通过 Linux 网络命名空间、挂载命名空间与路由/防火墙隔离，为其构造 SteamDeck
（`Jupiter` / `SteamOS`）运行环境。

`acc-gw.router.amd64` 等文件的著作权归雷神所有，使用前请遵守其服务条款。
本仓库代码仅供学习研究，请遵守当地法律法规。
