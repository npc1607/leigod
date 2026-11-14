#!/usr/bin/env bash
# 雷神加速器 (leigod-isolated) 全链路验证脚本
# 用法: sudo ./scripts/verify.sh

set -uo pipefail

PROJECT_DIR=$(CDPATH= cd -P "$(dirname "$0")/.." && pwd)
BIN="$PROJECT_DIR/bin/leigod"
NS=leigod-ns
TABLE=2540
VETH=leigod-host
ACC_LOG=/opt/leigod/runtime/log/acc_PC.log

PASS=0
FAIL=0
WARN=0

ok()   { printf '  \033[0;32m[PASS]\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[0;31m[FAIL]\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
warn() { printf '  \033[1;33m[WARN]\033[0m %s\n' "$*"; WARN=$((WARN+1)); }
hdr()  { printf '\n\033[1;36m--- %s ---\033[0m\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { echo "[ERROR] 请用 root 运行: sudo $0" >&2; exit 1; }

# ---------------------------------------------------------------- 1. 服务
hdr "1. systemd 服务"
if systemctl is-active --quiet leigod-isolated.service; then
    ok "leigod-isolated.service active"
else
    bad "leigod-isolated.service 未运行"
    journalctl -u leigod-isolated -n 20 --no-pager 2>/dev/null | sed 's/^/      /'
fi
if systemctl is-enabled --quiet leigod-isolated.service 2>/dev/null; then
    ok "开机自启已启用"
else
    warn "开机自启未启用"
fi
if getcap /opt/leigod/bin/leigod-nsenter 2>/dev/null | grep -q cap_sys_admin; then
    ok "leigod-nsenter 具备 cap_sys_admin（普通用户免 sudo 启动）"
else
    warn "leigod-nsenter 缺少 cap_sys_admin（Steam 启动项需要 sudo）"
fi

# ---------------------------------------------------------------- 2. 命名空间
hdr "2. 隔离网络命名空间"
if [ -e "/run/netns/$NS" ]; then
    ok "netns $NS 存在"
else
    bad "netns $NS 不存在"
fi
if ip -n "$NS" addr show 2>/dev/null | grep -q "10.254.254.2"; then
    ok "ns 内 veth 地址 10.254.254.2/24 已配置"
else
    bad "ns 内 veth 地址缺失"
fi
if ip -n "$NS" addr show wlan0 2>/dev/null | grep -q "inet "; then
    ok "ns 内 dummy wlan0 已配置 IP（$(ip -n "$NS" -4 -o addr show wlan0 2>/dev/null | awk '{print $4}')）"
else
    warn "ns 内 dummy wlan0 缺少 IP（手机 App 设备列表可能为空）"
fi
MAC=$(cat /var/lib/leigod/device-mac 2>/dev/null || echo "")
if [ -n "$MAC" ] && ip -n "$NS" link show wlan0 2>/dev/null | grep -qi "$MAC"; then
    ok "设备身份 MAC 一致: $MAC"
else
    bad "设备身份 MAC 不匹配（期望 $MAC）"
fi

# ---------------------------------------------------------------- 3. 策略路由
hdr "3. 宿主机策略路由（绕过 Throne TUN）"
if ip -4 rule show | grep -q "lookup $TABLE"; then
    ok "ip rule 指向 table $TABLE 已存在"
    ip -4 rule show | grep "lookup $TABLE" | sed 's/^/      /'
else
    bad "缺少 ip rule -> table $TABLE"
fi
if ip -4 route show table "$TABLE" 2>/dev/null | grep -q "^default via"; then
    ok "table $TABLE 默认路由已绑定物理网卡"
    ip -4 route show table "$TABLE" | sed 's/^/      /'
else
    bad "table $TABLE 缺少默认路由"
fi
if ip -4 route show table "$TABLE" 2>/dev/null | grep -q $(ip -4 addr show $(ip -4 route show table "$TABLE" | awk '/^default/{print $NF; exit}') 2>/dev/null | awk '/inet /{print $2; exit}' | cut -d/ -f1 || echo "__none__"); then
    : # 仅信息性检查，忽略
fi
WAN_DEV=$(ip -4 route show table "$TABLE" 2>/dev/null | awk '/^default/{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1); exit}')
if [ -n "$WAN_DEV" ]; then
    case "$WAN_DEV" in
        tun*|nekobox*|clash*|sing*|dae*|wg*|Meta*|throne*)
            bad "table $TABLE 默认路由走了代理网卡 $WAN_DEV（会被 Throne 二次代理）"
            ;;
        *)
            ok "table $TABLE 走物理网卡 $WAN_DEV（未经过代理 TUN）"
            ;;
    esac
fi

# ---------------------------------------------------------------- 4. 防火墙
hdr "4. 专属 iptables/ipset（不污染宿主机）"
if iptables -t nat -C POSTROUTING -j LEIGOD_NAT 2>/dev/null; then
    ok "LEIGOD_NAT 已挂载到 nat POSTROUTING"
else
    warn "LEIGOD_NAT 未挂载"
fi
if iptables -t filter -C FORWARD -j LEIGOD_FWD 2>/dev/null; then
    ok "LEIGOD_FWD 已挂载到 filter FORWARD"
else
    warn "LEIGOD_FWD 未挂载"
fi
MANGLE_OUT=$(iptables -t mangle -S OUTPUT 2>/dev/null | grep -c "10.254.254" | head -1)
MANGLE_OUT=${MANGLE_OUT:-0}
if [ "${MANGLE_OUT:-0}" -gt 0 ]; then
    bad "宿主机 mangle OUTPUT 出现 10.254.254 规则（说明雷神规则泄漏到宿主机）"
else
    ok "宿主机 mangle OUTPUT 干净（雷神规则被关在 ns 内）"
fi

# ---------------------------------------------------------------- 5. 进程
hdr "5. 雷神进程"
if pgrep -f 'acc-gw\.router\.amd64.*-r acc' >/dev/null 2>&1; then
    ok "加速进程 acc-gw -r acc 运行中"
else
    warn "加速进程未运行（需在手机 App 点「开始加速」）"
fi
if pgrep -f 'acc-gw\.router\.amd64.*-r daemon' >/dev/null 2>&1; then
    ok "守护进程 acc-gw -r daemon 运行中"
else
    bad "守护进程未运行"
fi
if pgrep -f 'leigod _ns-socks5' >/dev/null 2>&1; then
    ok "命名空间 SOCKS5 服务运行中"
else
    bad "命名空间 SOCKS5 服务未运行"
fi

# ---------------------------------------------------------------- 6. 加速隧道
hdr "6. 加速隧道"
if ip -n "$NS" link show 2>/dev/null | grep -qE 'tun_|tun[0-9]'; then
    TUNNAME=$(ip -n "$NS" -o link show 2>/dev/null | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -E '^tun' | head -1)
    ok "ns 内加速网卡存在: $TUNNAME"
else
    warn "ns 内无 tun 网卡（手机端未开始加速）"
fi
if [ -f "$ACC_LOG" ]; then
    DELAY=$(grep -o 'average_delay:[0-9]* ms' "$ACC_LOG" 2>/dev/null | tail -1 | grep -o '[0-9]*')
    LOSS=$(grep -o 'lost_persent:[0-9]*' "$ACC_LOG" 2>/dev/null | tail -1 | grep -o '[0-9]*')
    SERVER=$(grep -o 'send heart to server(fd:[0-9]*):[0-9.]*:[0-9]*' "$ACC_LOG" 2>/dev/null | tail -1 | sed 's/.*)://')
    if [ -n "${DELAY:-}" ]; then
        ok "隧道心跳正常  节点=$SERVER  延迟=${DELAY}ms  丢包=${LOSS}%"
        if [ "${DELAY:-999}" -lt 80 ]; then
            ok "延迟 ${DELAY}ms 处于加速区间"
        else
            warn "延迟 ${DELAY}ms 偏高"
        fi
    else
        warn "日志中未找到心跳记录"
    fi
    GAME_HITS=$(grep -c 'go acc' "$ACC_LOG" 2>/dev/null | head -1)
    GAME_HITS=${GAME_HITS:-0}
    case "$GAME_HITS" in ''|*[!0-9]*) GAME_HITS=0 ;; esac
    if [ "${GAME_HITS:-0}" -gt 0 ]; then
        ok "已命中加速规则 $GAME_HITS 次（域名/IP 走雷神节点）"
        grep 'go acc' "$ACC_LOG" 2>/dev/null | tail -3 | sed 's/^/      /'
    else
        warn "尚未命中加速规则（还没启动游戏或 App 未加速）"
    fi
else
    warn "未找到加速日志 $ACC_LOG"
fi

# ---------------------------------------------------------------- 7. Steam IPC
hdr "7. Steam IPC 桥（解决 SteamAPI_Init 失败）"
if ss -ltnp 2>/dev/null | grep -q "127.0.0.1:57343"; then
    ok "宿主机 127.0.0.1:57343 Steam 客户端在监听"
else
    bad "宿主机 57343 未监听（Steam 没启动？）"
fi
# 注意：10.254.254.1 属于宿主机命名空间，必须在【宿主机】侧查，
# 在 ns 内查永远看不到（旧版本的误报修复）。
if ss -ltn 2>/dev/null | grep -q "10.254.254.1:57343"; then
    ok "宿主机侧桥接端口 10.254.254.1:57343 已监听"
else
    bad "宿主机侧桥接 10.254.254.1:57343 未监听"
fi
if ip netns exec "$NS" ss -ltn 2>/dev/null | grep -q "127.0.0.1:57343"; then
    ok "命名空间内 127.0.0.1:57343 已监听（可转到宿主机 Steam）"
else
    bad "命名空间内 127.0.0.1:57343 未监听 → SteamAPI_Init 会失败"
fi

echo "      正在实测 ns 内 Steam API 初始化..."
STEAM_OUT=$(ip netns exec "$NS" python3 -c '
import ctypes, os
os.environ["SteamAppId"] = "238960"
try:
    lib = ctypes.CDLL("/home/pcong/.steam/sdk64/steamclient.so")
    lib.Steam_CreateSteamPipe.restype = ctypes.c_int
    lib.Steam_ConnectToGlobalUser.argtypes = [ctypes.c_int]
    lib.Steam_ConnectToGlobalUser.restype = ctypes.c_int
    pipe = lib.Steam_CreateSteamPipe()
    user = lib.Steam_ConnectToGlobalUser(pipe)
    print(f"pipe={pipe} user={user}")
except Exception as e:
    print(f"error={e}")
' 2>&1 | tail -1)
if echo "$STEAM_OUT" | grep -q "pipe=[1-9]"; then
    ok "ns 内 SteamAPI 可用（$STEAM_OUT）"
else
    bad "ns 内 SteamAPI 仍不可用（$STEAM_OUT）"
fi

# ---------------------------------------------------------------- 8. 出口连通
hdr "8. 隔离命名空间出口连通性"
for target in "www.pathofexile.com" "www.baidu.com"; do
    OUT=$(timeout 12 ip netns exec "$NS" getent hosts "$target" 2>/dev/null | head -1)
    if [ -n "$OUT" ]; then
        ok "DNS 解析 $target -> ${OUT#* }"
    else
        warn "DNS 解析失败: $target"
    fi
done
HTTP=$(timeout 15 ip netns exec "$NS" curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://www.pathofexile.com 2>/dev/null || echo "000")
if [ "$HTTP" = "200" ] || [ "$HTTP" = "301" ] || [ "$HTTP" = "302" ]; then
    ok "ns 内 HTTPS 访问 pathofexile.com 成功（HTTP $HTTP）"
else
    warn "ns 内 HTTPS 访问 pathofexile.com 返回 $HTTP"
fi

# ---------------------------------------------------------------- 9. SOCKS5
hdr "9. SOCKS5 出站（给 Throne 分流用）"
if ss -ltnp 2>/dev/null | grep -q "127.0.0.1:15588"; then
    ok "宿主机 127.0.0.1:15588 已监听"
else
    bad "宿主机 127.0.0.1:15588 未监听"
fi
if ip netns exec "$NS" ss -ltnu 2>/dev/null | grep -q "10.254.254.2:1080"; then
    ok "ns 内 SOCKS5 10.254.254.2:1080 已监听"
else
    bad "ns 内 SOCKS5 未监听"
fi
SOCKS_HTTP=$(timeout 20 curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -x socks5h://127.0.0.1:15588 https://www.pathofexile.com 2>/dev/null || echo "000")
if [ "$SOCKS_HTTP" = "200" ] || [ "$SOCKS_HTTP" = "301" ] || [ "$SOCKS_HTTP" = "302" ]; then
    ok "经 SOCKS5 访问 pathofexile.com 成功（HTTP $SOCKS_HTTP）"
else
    bad "经 SOCKS5 访问失败（HTTP $SOCKS_HTTP）"
fi

# ---------------------------------------------------------------- 10. 游戏进程
hdr "10. 游戏进程（PoE）"
GAME_PIDS=$(pgrep -f 'PathOfExileSteam\.exe' 2>/dev/null | tr '\n' ' ' || true)
if [ -z "${GAME_PIDS// /}" ]; then
    warn "未检测到 PathOfExileSteam.exe（游戏未启动）"
else
    ok "游戏进程运行中: $GAME_PIDS"
    HOST_NS=$(readlink /proc/self/ns/net 2>/dev/null)
    IN_NS=0
    TOTAL=0
    for p in $GAME_PIDS; do
        TOTAL=$((TOTAL+1))
        pns=$(readlink "/proc/$p/ns/net" 2>/dev/null || echo "")
        if [ -n "$pns" ] && [ -n "$HOST_NS" ] && [ "$pns" != "$HOST_NS" ]; then
            IN_NS=$((IN_NS+1))
        fi
    done
    if [ "$IN_NS" -gt 0 ]; then
        ok "$IN_NS/$TOTAL 个游戏进程已进入隔离命名空间（流量被雷神接管）"
    else
        bad "游戏进程仍在宿主机命名空间（未通过 leigod run 启动）"
    fi
fi
CLIENT_LOG="/mnt/ntfs-game/SteamLibrary/steamapps/common/Path of Exile/logs/Client.txt"
if [ -f "$CLIENT_LOG" ]; then
    LAST_SESSION=$(awk '/LOG FILE OPENING/{buf=""} {buf=buf"\n"$0} END{print buf}' "$CLIENT_LOG" 2>/dev/null)
    if [ -n "$LAST_SESSION" ]; then
        if echo "$LAST_SESSION" | grep -q "Unable to init Steam API"; then
            bad "最近一次启动 SteamAPI 初始化失败（Steam IPC 桥未生效）"
        else
            ok "最近一次启动 SteamAPI 初始化正常（无 Unable to init Steam API）"
        fi
        INST=$(echo "$LAST_SESSION" | grep -o 'Connecting to instance server at [0-9.]*:[0-9]*' | tail -1)
        [ -n "$INST" ] && ok "已连上游戏实例: ${INST#Connecting to instance server at }"
    fi
fi

# ---------------------------------------------------------------- 汇总
printf '\n\033[1m================ 验证汇总 ================\033[0m\n'
printf '  PASS: \033[0;32m%d\033[0m   WARN: \033[1;33m%d\033[0m   FAIL: \033[0;31m%d\033[0m\n' "$PASS" "$WARN" "$FAIL"
if [ "$FAIL" -eq 0 ]; then
    printf '  结论: \033[0;32m链路完整，可以启动游戏\033[0m\n'
    printf '  Steam 启动项: leigod run -- %%command%%\n'
else
    printf '  结论: \033[0;31m存在 %d 项失败，请查看上面 FAIL 项\033[0m\n' "$FAIL"
fi
printf '==========================================\n'

exit 0
