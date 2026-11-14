#!/usr/bin/env bash
# 雷神加速器 (leigod-isolated) 独立一键卸载与网络清理脚本
# 用法:
#   sudo ./scripts/uninstall.sh           # 卸载服务、清理网络命名空间和规则（保留设备绑定身份）
#   sudo ./scripts/uninstall.sh --purge   # 彻底卸载并删除所有配置与绑定身份 (/var/lib/leigod, /etc/leigod)

set -euo pipefail

PURGE=0
INSTALL_DIR="${LEIGOD_INSTALL_DIR:-/opt/leigod}"
STATE_DIR="${LEIGOD_STATE_DIR:-/var/lib/leigod}"
CONFIG_DIR="/etc/leigod"
NAMESPACE="${LEIGOD_NAMESPACE:-leigod-ns}"
VETH_HOST="${LEIGOD_VETH_HOST:-leigod-host}"
VETH_SUBNET="${LEIGOD_VETH_SUBNET:-10.254.254.0/24}"
ROUTE_TABLE="${LEIGOD_ROUTE_TABLE:-2540}"
RULE_PREF="${LEIGOD_RULE_PREF:-90}"
RULE_PREF_IN=$((RULE_PREF - 1))

for arg in "$@"; do
    case "$arg" in
        --purge|-p)
            PURGE=1
            ;;
        -h|--help)
            echo "Usage: sudo $0 [--purge]"
            echo "  --purge, -p   同时删除 /var/lib/leigod (绑定身份/MAC) 和 /etc/leigod 配置目录"
            exit 0
            ;;
        *)
            echo "[ERROR] Unknown argument: $arg" >&2
            exit 1
            ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "[ERROR] 请使用 root 权限运行: sudo $0 $*" >&2
    exit 1
fi

info() { printf '\033[0;32m[INFO]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[WARN]\033[0m %s\n' "$*" >&2; }

# 1. 如果保留身份，先备份现有的 accelerator.ini 绑定 token
if [ "$PURGE" -eq 0 ] && [ -f "$INSTALL_DIR/runtime/config/accelerator.ini" ]; then
    if grep -q '^token=..*' "$INSTALL_DIR/runtime/config/accelerator.ini" 2>/dev/null; then
        mkdir -p "$STATE_DIR"
        cp -f "$INSTALL_DIR/runtime/config/accelerator.ini" "$STATE_DIR/accelerator.ini.bak"
        chmod 600 "$STATE_DIR/accelerator.ini.bak"
        info "已备份雷神账号绑定状态至 $STATE_DIR/accelerator.ini.bak"
    fi
fi

# 2. 停止并移除 systemd 服务
for svc in leigod-isolated.service leigod_plugin.service leigod.service; do
    if systemctl list-unit-files "$svc" >/dev/null 2>&1 || [ -f "/etc/systemd/system/$svc" ]; then
        info "正在停止并禁用服务: $svc ..."
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
        rm -f "/etc/systemd/system/$svc"
    fi
done
systemctl daemon-reload 2>/dev/null || true
systemctl reset-failed 2>/dev/null || true

# 3. 终止残留的雷神进程
info "正在检查并清理残留进程..."
for pattern in "acc-gw\.router\.amd64" "acc_upgrade_monitor" "steamdeck_acc_monitor" "leigod.*_ns-"; do
    pkill -TERM -f "$pattern" 2>/dev/null || true
done
sleep 0.3
for pattern in "acc-gw\.router\.amd64" "acc_upgrade_monitor" "steamdeck_acc_monitor" "leigod.*_ns-"; do
    pkill -KILL -f "$pattern" 2>/dev/null || true
done

# 4. 清理宿主机策略路由规则 (table 2540)
info "正在清理宿主机策略路由表 ($ROUTE_TABLE)..."
for _ in 1 2 3; do
    ip -4 rule del from "$VETH_SUBNET" pref "$RULE_PREF" lookup "$ROUTE_TABLE" 2>/dev/null || true
    ip -4 rule del to "$VETH_SUBNET" pref "$RULE_PREF_IN" lookup "$ROUTE_TABLE" 2>/dev/null || true
done
ip -4 route flush table "$ROUTE_TABLE" 2>/dev/null || true

# 5. 清理宿主机专属 iptables 链 (LEIGOD_NAT / LEIGOD_FWD)
info "正在清理宿主机专属 iptables 链 (LEIGOD_NAT / LEIGOD_FWD)..."
for _ in 1 2 3; do
    iptables -t nat -D POSTROUTING -j LEIGOD_NAT 2>/dev/null || break
done
iptables -t nat -F LEIGOD_NAT 2>/dev/null || true
iptables -t nat -X LEIGOD_NAT 2>/dev/null || true

for _ in 1 2 3; do
    iptables -t filter -D FORWARD -j LEIGOD_FWD 2>/dev/null || break
done
iptables -t filter -F LEIGOD_FWD 2>/dev/null || true
iptables -t filter -X LEIGOD_FWD 2>/dev/null || true

# 6. 删除 veth 虚拟网卡与隔离网络命名空间
info "正在移除隔离网络命名空间 ($NAMESPACE) 与虚拟网卡 ($VETH_HOST)..."
ip link delete "$VETH_HOST" 2>/dev/null || true
if [ -e "/run/netns/$NAMESPACE" ]; then
    for pid in $(ip netns pids "$NAMESPACE" 2>/dev/null || true); do
        kill -KILL "$pid" 2>/dev/null || true
    done
    ip netns delete "$NAMESPACE" 2>/dev/null || true
fi
rm -rf "/etc/netns/$NAMESPACE"

# 如果宿主机上存在旧版创建的 dummy wlan0，一并安全移除
if ip -d link show wlan0 2>/dev/null | grep -qw dummy; then
    ip link delete wlan0 2>/dev/null || true
fi

# 7. 删除程序目录与命令软链接
info "正在删除安装目录 ($INSTALL_DIR) 与软链接..."
rm -f /usr/local/bin/leigod
if [ -L /home/leigod ]; then
    rm -f /home/leigod
fi
rm -rf "$INSTALL_DIR" /run/leigod /tmp/acc

# 8. 如果指定了 --purge，彻底清除持久化状态和配置
if [ "$PURGE" -eq 1 ]; then
    info "正在彻底清除配置文件 ($CONFIG_DIR) 与设备绑定身份 ($STATE_DIR)..."
    rm -rf "$STATE_DIR" "$CONFIG_DIR"
    info "雷神加速器 (leigod-isolated) 已完全卸载并清除所有数据！"
else
    info "雷神加速器 (leigod-isolated) 已卸载！(设备身份与绑定备份保留在 $STATE_DIR，如需彻底删除请加 --purge)"
fi
