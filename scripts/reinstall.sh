#!/usr/bin/env bash
# 雷神加速器 (leigod-isolated) 重新安装脚本
# 用法:
#   sudo ./scripts/reinstall.sh            # 保留设备身份/手机绑定，重装并重启服务
#   sudo ./scripts/reinstall.sh --purge    # 彻底清空设备身份和配置后重装（手机需重新绑定）

set -euo pipefail

PROJECT_DIR=$(CDPATH= cd -P "$(dirname "$0")/.." && pwd)
BIN="$PROJECT_DIR/bin/leigod"
PURGE=0

for arg in "$@"; do
    case "$arg" in
        --purge|-p) PURGE=1 ;;
        -h|--help)
            echo "Usage: sudo $0 [--purge]"
            exit 0
            ;;
        *) echo "[ERROR] unknown argument: $arg" >&2; exit 1 ;;
    esac
done

info() { printf '\033[0;32m[INFO]\033[0m %s\n' "$*"; }
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { echo "[ERROR] 请用 root 运行: sudo $0 $*" >&2; exit 1; }

# sudo 会重置 PATH（secure_path），go 通常装在 /usr/local/go/bin，这里补回来
for _p in /usr/local/go/bin /usr/lib/go/bin /usr/local/bin "${SUDO_USER:+/home/$SUDO_USER/go/bin}" "${HOME:-/root}/go/bin"; do
    [ -d "$_p" ] && case ":$PATH:" in *":$_p:"*) ;; *) PATH="$_p:$PATH" ;; esac
done
export PATH

step "1/6 编译最新二进制"
if command -v go >/dev/null 2>&1; then
    info "使用 $(command -v go) ($(go version 2>/dev/null | awk '{print $3}'))"
    # 以调用者身份编译，避免 root 拥有构建产物导致后续 git/make 权限问题
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
        sudo -u "$SUDO_USER" env PATH="$PATH" make -C "$PROJECT_DIR" build
    else
        make -C "$PROJECT_DIR" build
    fi
elif [ -x "$BIN" ]; then
    info "未找到 go，跳过编译，使用已有二进制: $BIN"
    info "（如需重新编译请先安装 go，或手动执行 make build）"
else
    echo "[ERROR] 未找到 go 编译器，且 $BIN 不存在，无法继续" >&2
    echo "        请安装 go 后重试：sudo pacman -S go  或  https://go.dev/dl/" >&2
    exit 1
fi

step "2/6 备份手机绑定状态"
if [ -f /opt/leigod/runtime/config/accelerator.ini ] && grep -q '^token=..*' /opt/leigod/runtime/config/accelerator.ini 2>/dev/null; then
    mkdir -p /var/lib/leigod
    cp -f /opt/leigod/runtime/config/accelerator.ini /var/lib/leigod/accelerator.ini.bak
    chmod 600 /var/lib/leigod/accelerator.ini.bak
    info "已备份绑定 token 到 /var/lib/leigod/accelerator.ini.bak"
else
    info "当前无有效绑定 token（需在手机 App 重新绑定）"
fi

step "3/6 停止服务并卸载旧版本"
if [ "$PURGE" -eq 1 ]; then
    "$BIN" uninstall --purge || true
else
    "$BIN" uninstall || true
fi

step "4/6 清理残留"
systemctl reset-failed leigod-isolated.service 2>/dev/null || true
pkill -KILL -f 'acc-gw\.router' 2>/dev/null || true
ip link delete leigod-host 2>/dev/null || true
ip netns delete leigod-ns 2>/dev/null || true
rm -rf /etc/netns/leigod-ns
info "残留清理完成"

step "5/6 全新安装并启动"
"$BIN" install

step "6/6 等待服务就绪"
for i in $(seq 1 30); do
    if systemctl is-active --quiet leigod-isolated.service; then
        break
    fi
    sleep 1
done
sleep 3

echo ""
"$BIN" status

echo ""
info "重装完成。下一步："
echo "  1) 手机雷神 App 里重新点一次「开始加速」（服务重启后加速会话需要重新下发）"
echo "  2) 然后运行: sudo $PROJECT_DIR/scripts/verify.sh"
