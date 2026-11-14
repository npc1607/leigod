package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"leigod/internal/assets"
	"leigod/internal/config"
	"leigod/internal/monitor"
	"leigod/internal/netns"
)

const (
	ServiceName = "leigod-isolated.service"
	ServicePath = "/etc/systemd/system/" + ServiceName
	DefaultYAML = "/etc/leigod/config.yaml"
)

// App coordinates installation, service management, namespace daemon, and game runner.
type App struct {
	Cfg config.Config
	Out io.Writer
	Err io.Writer
}

// New creates an App instance with standard output streams.
func New(cfg config.Config) *App {
	return &App{
		Cfg: cfg,
		Out: os.Stdout,
		Err: os.Stderr,
	}
}

func (a *App) logf(format string, args ...any) {
	if a.Out != nil {
		fmt.Fprintf(a.Out, "[leigod] "+format+"\n", args...)
	}
}

// Install sets up the binary, capability helper, Leigod assets, and systemd service.
func (a *App) Install(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install requires root privileges (run with sudo)")
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("only x86_64/amd64 is supported (current: %s)", runtime.GOARCH)
	}

	if err := a.ensureSystemDeps(); err != nil {
		return err
	}

	// Stop existing service if running before replacing binaries
	_ = exec.Command("systemctl", "stop", ServiceName).Run()

	// 1. Download and verify Leigod official assets
	if err := assets.EnsureAssets(a.Cfg.InstallDir, a.Cfg.StateDir, a.Cfg.MirrorURL, a.Cfg.DNSServers, a.Cfg.SkipVerify, a.logf); err != nil {
		return err
	}

	// 2. Install our own binary and the cap_sys_admin nsenter helper
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	binDir := filepath.Join(a.Cfg.InstallDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	installedBin := filepath.Join(binDir, "leigod")
	helperBin := filepath.Join(binDir, "leigod-nsenter")

	if err := copyBinary(selfExe, installedBin, 0o755); err != nil {
		return fmt.Errorf("install %s: %w", installedBin, err)
	}
	if err := copyBinary(selfExe, helperBin, 0o755); err != nil {
		return fmt.Errorf("install %s: %w", helperBin, err)
	}
	if setcapPath, err := exec.LookPath("setcap"); err == nil {
		if err := exec.Command(setcapPath, "cap_sys_admin+ep", helperBin).Run(); err != nil {
			a.logf("warning: setcap on %s failed: %v", helperBin, err)
		} else {
			a.logf("configured capability helper %s (steam launch works without sudo)", helperBin)
		}
	}

	// Symlink /usr/local/bin/leigod -> <InstallDir>/bin/leigod
	_ = os.MkdirAll("/usr/local/bin", 0o755)
	_ = os.Remove("/usr/local/bin/leigod")
	if err := os.Symlink(installedBin, "/usr/local/bin/leigod"); err != nil {
		a.logf("warning: create symlink /usr/local/bin/leigod: %v", err)
	}

	// 3. Write /etc/leigod/config.yaml if not present
	if err := os.MkdirAll(filepath.Dir(DefaultYAML), 0o755); err == nil {
		if _, err := os.Stat(DefaultYAML); os.IsNotExist(err) {
			_ = os.WriteFile(DefaultYAML, []byte(RenderDefaultYAML(a.Cfg)), 0o644)
		}
	}

	// 4. Write and enable systemd service
	unitContent := RenderSystemdUnit(installedBin, DefaultYAML)
	if err := os.WriteFile(ServicePath, []byte(unitContent), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", ServicePath, err)
	}
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "enable", "--now", ServiceName).Run(); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w", ServiceName, err)
	}

	mac, _ := assets.DeriveOrLoadDeviceMAC(a.Cfg.DeviceMAC, a.Cfg.StateDir, "/etc/machine-id")
	a.printInstallSummary(mac)
	return nil
}

// Uninstall stops the service, cleans up the namespace and rules, and removes installed files.
func (a *App) Uninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("uninstall requires root privileges (run with sudo)")
	}
	if !a.Cfg.Purge {
		assets.BackupBindingState(a.Cfg.InstallDir, a.Cfg.StateDir)
	}

	a.logf("stopping and disabling %s...", ServiceName)
	_ = exec.Command("systemctl", "stop", ServiceName).Run()
	_ = exec.Command("systemctl", "disable", ServiceName).Run()
	_ = os.Remove(ServicePath)
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = exec.Command("systemctl", "reset-failed").Run()

	// Ensure any stray acc-gw processes are terminated
	killAccProcesses()

	a.logf("cleaning up network namespace %s, veth %s, policy routing table %d, and iptables chains...",
		a.Cfg.Namespace, a.Cfg.VethHost, a.Cfg.RouteTable)
	nsMgr := netns.NewManager(a.Cfg, a.logf)
	_ = nsMgr.Teardown()

	if target, err := os.Readlink("/usr/local/bin/leigod"); err == nil && strings.HasPrefix(target, a.Cfg.InstallDir) {
		_ = os.Remove("/usr/local/bin/leigod")
	} else if _, err := os.Lstat("/usr/local/bin/leigod"); err == nil {
		_ = os.Remove("/usr/local/bin/leigod")
	}

	_ = os.RemoveAll(a.Cfg.InstallDir)

	if a.Cfg.Purge {
		_ = os.RemoveAll(a.Cfg.StateDir)
		_ = os.RemoveAll(filepath.Dir(DefaultYAML))
		a.logf("uninstalled and purged all Leigod files, configs (%s), and device identity (%s)", filepath.Dir(DefaultYAML), a.Cfg.StateDir)
	} else {
		a.logf("uninstalled Leigod isolated service (device identity preserved in %s; use --purge to remove all state)", a.Cfg.StateDir)
	}
	return nil
}

// Start starts the systemd service.
func (a *App) Start() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("start requires root privileges (run with sudo)")
	}
	return exec.Command("systemctl", "start", ServiceName).Run()
}

// Stop stops the systemd service.
func (a *App) Stop() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("stop requires root privileges (run with sudo)")
	}
	return exec.Command("systemctl", "stop", ServiceName).Run()
}

// Restart restarts the systemd service.
func (a *App) Restart() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("restart requires root privileges (run with sudo)")
	}
	return exec.Command("systemctl", "restart", ServiceName).Run()
}

// Status prints the current health of the service, namespace, account binding, and acceleration tunnel.
func (a *App) Status() error {
	paths := assets.ResolvePaths(a.Cfg.InstallDir)
	mac, _ := assets.DeriveOrLoadDeviceMAC(a.Cfg.DeviceMAC, a.Cfg.StateDir, "/etc/machine-id")

	svcState := "inactive"
	if out, err := exec.Command("systemctl", "is-active", ServiceName).Output(); err == nil {
		svcState = strings.TrimSpace(string(out))
	}

	bound := false
	if data, err := os.ReadFile(paths.AcceleratorINI); err == nil {
		bound = assets.HasBindingToken(string(data))
	}

	nsMgr := netns.NewManager(a.Cfg, nil)
	nsActive := nsMgr.NamespaceExists()
	tunName, tunActive := nsMgr.AccTunActive()
	wan, wanErr := nsMgr.DetectWAN()

	fmt.Fprintf(a.Out, "=== Leigod Isolated Accelerator Status ===\n")
	fmt.Fprintf(a.Out, "Service (%s) : %s\n", ServiceName, svcState)
	fmt.Fprintf(a.Out, "Network Namespace       : %s (active: %v)\n", a.Cfg.Namespace, nsActive)
	fmt.Fprintf(a.Out, "Device Identity (MAC/SN): %s\n", mac)
	fmt.Fprintf(a.Out, "Mobile App Bound        : %v\n", bound)
	if wanErr == nil {
		fmt.Fprintf(a.Out, "Physical WAN Bypass     : %s (gw %s, LAN IP %s, route table %d)\n",
			wan.Interface, wan.Gateway, wan.LocalIP, a.Cfg.RouteTable)
	}
	if tunActive {
		fmt.Fprintf(a.Out, "Game Acceleration TUN   : ACTIVE (%s inside %s)\n", tunName, a.Cfg.Namespace)
	} else {
		fmt.Fprintf(a.Out, "Game Acceleration TUN   : standby (start acceleration in Leigod mobile app)\n")
	}
	fmt.Fprintf(a.Out, "Throne/Sing-box SOCKS5  : %s (or direct %s)\n", a.Cfg.Socks5Listen, a.Cfg.NSSocks5Listen)
	fmt.Fprintf(a.Out, "Steam Launch Option     : leigod run -- %%command%%\n")

	mon := monitor.New(monitor.Config{
		LogPath:     paths.LogDir + "/acc_PC.log",
		Namespace:   a.Cfg.Namespace,
		ServiceName: ServiceName,
		RouteTable:  a.Cfg.RouteTable,
		InstallDir:  a.Cfg.InstallDir,
		StateDir:    a.Cfg.StateDir,
		WebPort:     a.Cfg.WebPort,
		WebListen:   a.Cfg.WebListen,
	})
	snap := mon.Snapshot()
	gr := snap.GameRTT
	fmt.Fprintf(a.Out, "\n游戏链路真实 RTT (TCP):\n")
	if !gr.Available || (gr.AccCount == 0 && gr.DirCount == 0) {
		fmt.Fprintf(a.Out, "  (无) %s\n", gr.Note)
	} else {
		fmt.Fprintf(a.Out, "  最差 %.1fms  最好 %.1fms  平均 %.1fms  加速 %d 条 / 直连 %d 条\n",
			gr.WorstMs, gr.BestMs, gr.AvgMs, gr.AccCount, gr.DirCount)
		for _, c := range gr.Conns {
			if c.Role != "middleware" || (c.Kind != "game_server" && c.Kind != "leigod_node") {
				continue
			}
			kind := "直连服务器"
			if c.Kind == "leigod_node" {
				kind = "雷神节点"
			}
			fmt.Fprintf(a.Out, "    %-24s %-12s rtt=%6.1fms minrtt=%6.1fms retrans=%d unacked=%d\n",
				c.Peer, kind, c.RTTms, c.MinRTTms, c.Retrans, c.Unacked)
		}
	}
	return nil
}

func (a *App) ensureSystemDeps() error {
	// Ensure ipset is installed on Arch/Debian/Fedora if missing
	if _, err := exec.LookPath("ipset"); err != nil {
		if pacman, err := exec.LookPath("pacman"); err == nil {
			a.logf("installing missing dependency: ipset ...")
			cmd := exec.Command(pacman, "-S", "--noconfirm", "--needed", "ipset")
			cmd.Stdout = a.Out
			cmd.Stderr = a.Err
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("install ipset via pacman: %w", err)
			}
		} else {
			return fmt.Errorf("missing required command: ipset (please install ipset)")
		}
	}
	for _, bin := range []string{"ip", "iptables"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("missing required command: %s", bin)
		}
	}
	return nil
}

func copyBinary(src, dst string, mode os.FileMode) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return os.Chmod(dst, mode)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (a *App) printInstallSummary(mac string) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\n============================================================\n")
	fmt.Fprintf(&b, " Leigod Isolated Accelerator Installed & Running!\n")
	fmt.Fprintf(&b, "============================================================\n")
	fmt.Fprintf(&b, " 1. 手机雷神 App 绑定 (手机与台式机连同一路由器):\n")
	fmt.Fprintf(&b, "    打开雷神加速器 App -> 硬件加速 -> SteamDeck -> 扫描绑定\n")
	fmt.Fprintf(&b, "    (设备 SN/MAC: %s)\n\n", mac)
	fmt.Fprintf(&b, " 2. 加速方式一 (Steam 启动项按进程隔离, 推荐):\n")
	fmt.Fprintf(&b, "    在 Steam 游戏属性 -> 启动选项中填入:\n")
	fmt.Fprintf(&b, "      leigod run -- %%command%%\n\n")
	fmt.Fprintf(&b, " 3. 加速方式二 (与 Throne / Sing-box 联动分流):\n")
	fmt.Fprintf(&b, "    在 Throne 中新建 SOCKS5 出站: 127.0.0.1:15588 (或 %s)\n", a.Cfg.NSSocks5Listen)
	fmt.Fprintf(&b, "    在路由设置中将游戏进程名 (process_name) 分流至该出站。\n")
	fmt.Fprintf(&b, "============================================================\n")
	_, _ = a.Out.Write(b.Bytes())
}

// RenderSystemdUnit generates the systemd unit file for leigod-isolated.service.
func RenderSystemdUnit(binPath, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Leigod Accelerator in Isolated Network Namespace (Throne/Sing-box compatible)
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=%s --config %s daemon
Restart=on-failure
RestartSec=3
KillMode=control-group
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
`, binPath, configPath)
}

// RenderDefaultYAML produces the default /etc/leigod/config.yaml content.
func RenderDefaultYAML(cfg config.Config) string {
	return fmt.Sprintf(`# Leigod Isolated Network Namespace Configuration
install_dir: %s
state_dir: %s
namespace: %s
veth_host: %s
veth_ns: %s
host_veth_cidr: %s
ns_veth_cidr: %s
route_table: %d
rule_pref: %d
wan_interface: %s
device_mac: %s
device_ip: %s
mirror_url: %s
web_port: %d
discovery_port: %d
socks5_listen: %s
ns_socks5_listen: %s
dns_servers: %s
skip_verify: %v
`,
		cfg.InstallDir,
		cfg.StateDir,
		cfg.Namespace,
		cfg.VethHost,
		cfg.VethNS,
		cfg.HostVethCIDR,
		cfg.NSVethCIDR,
		cfg.RouteTable,
		cfg.RulePref,
		cfg.WANInterface,
		cfg.DeviceMAC,
		cfg.DeviceIP,
		cfg.MirrorURL,
		cfg.WebPort,
		cfg.DiscoveryPort,
		cfg.Socks5Listen,
		cfg.NSSocks5Listen,
		strings.Join(cfg.DNSServers, ", "),
		cfg.SkipVerify,
	)
}
