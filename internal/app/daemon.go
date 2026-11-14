package app

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"leigod/internal/assets"
	"leigod/internal/bridge"
	"leigod/internal/netns"
	"leigod/internal/socks5"
)

const (
	nobodyUID        = 65534
	nobodyGID        = 65534
	maxDaemonLogSize = 50 * 1024 * 1024 // 50 MiB
)

// RunDaemon orchestrates the host-side setup, bridges, and namespace worker processes.
func (a *App) RunDaemon(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("daemon must be run as root")
	}

	// Best-effort load required kernel modules on host
	_ = exec.Command("modprobe", "tun").Run()
	_ = exec.Command("modprobe", "dummy").Run()

	deviceMAC, err := assets.DeriveOrLoadDeviceMAC(a.Cfg.DeviceMAC, a.Cfg.StateDir, "/etc/machine-id")
	if err != nil {
		return fmt.Errorf("derive device MAC: %w", err)
	}
	a.logf("using persistent SteamDeck device MAC (SN): %s", deviceMAC)

	if err := assets.EnsureAssets(a.Cfg.InstallDir, a.Cfg.StateDir, a.Cfg.MirrorURL, a.Cfg.DNSServers, a.Cfg.SkipVerify, a.logf); err != nil {
		return fmt.Errorf("ensure assets: %w", err)
	}

	nsMgr := netns.NewManager(a.Cfg, a.logf)
	wan, err := nsMgr.Setup(deviceMAC)
	if err != nil {
		return fmt.Errorf("setup network namespace: %w", err)
	}
	defer func() {
		assets.BackupBindingState(a.Cfg.InstallDir, a.Cfg.StateDir)
		a.logf("cleaning up network namespace %s and host routing rules...", a.Cfg.Namespace)
		_ = nsMgr.Teardown()
	}()

	nsIP, err := a.Cfg.NSVethIP()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 5)

	// 1. Start UDP 6066 LAN broadcast discovery relay (Host 0.0.0.0:6066 -> NS 10.254.254.2:6066)
	udpRelay := &bridge.UDPRelay{
		ListenAddr: fmt.Sprintf("0.0.0.0:%d", a.Cfg.DiscoveryPort),
		TargetAddr: fmt.Sprintf("%s:%d", nsIP, a.Cfg.DiscoveryPort),
		Logf:       a.logf,
	}
	go func() {
		if err := udpRelay.Start(ctx); err != nil {
			errCh <- fmt.Errorf("udp discovery relay: %w", err)
		}
	}()

	// 2. Start TCP 5588 WebSocket guarded proxy (Host 0.0.0.0:5588 -> NS 10.254.254.2:5588)
	wsProxy := &bridge.WSGuardProxy{
		ListenAddr: fmt.Sprintf("0.0.0.0:%d", a.Cfg.WebPort),
		TargetAddr: fmt.Sprintf("%s:%d", nsIP, a.Cfg.WebPort),
		Logf:       a.logf,
	}
	go func() {
		if err := wsProxy.Start(ctx); err != nil {
			errCh <- fmt.Errorf("websocket guard proxy: %w", err)
		}
	}()

	// 3. Start host SOCKS5 forwarder (e.g. 127.0.0.1:15588 -> NS 10.254.254.2:1080)
	if a.Cfg.Socks5Listen != "" && a.Cfg.Socks5Listen != a.Cfg.NSSocks5Listen {
		socksForwarder := &bridge.TCPProxy{
			ListenAddr: a.Cfg.Socks5Listen,
			TargetAddr: a.Cfg.NSSocks5Listen,
			Logf:       a.logf,
		}
		go func() {
			if err := socksForwarder.Start(ctx); err != nil {
				errCh <- fmt.Errorf("host socks5 forwarder: %w", err)
			}
		}()
	}

	// 3b. Start Steam IPC bridge (Host 10.254.254.1:57343 -> Host 127.0.0.1:57343)
	// so Steam games running inside leigod-ns can reach the host Steam client (127.0.0.1:57343).
	hostIP, err := a.Cfg.HostVethIP()
	if err == nil {
		steamHostBridge := &bridge.TCPProxy{
			ListenAddr: net.JoinHostPort(hostIP, "57343"),
			TargetAddr: "127.0.0.1:57343",
			Logf:       a.logf,
		}
		go func() {
			_ = steamHostBridge.Start(ctx)
		}()
	}

	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	// 4. Start unprivileged SOCKS5 server inside leigod-ns (runs as UID 65534 so Leigod TUN intercepts its traffic)
	socksCmd := exec.CommandContext(ctx, "ip", "netns", "exec", a.Cfg.Namespace,
		selfExe, "_ns-socks5",
		"--ns-socks5-listen", a.Cfg.NSSocks5Listen,
		"--ns-veth-cidr", a.Cfg.NSVethCIDR,
	)
	socksCmd.Stdout = os.Stdout
	socksCmd.Stderr = os.Stderr
	if err := socksCmd.Start(); err != nil {
		return fmt.Errorf("start namespace socks5 server: %w", err)
	}
	go func() {
		if err := socksCmd.Wait(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("namespace socks5 server exited: %w", err)
		}
	}()

	// 5. Start sandboxed Leigod monitor inside leigod-ns with private Mount & PID namespaces
	nsDaemonCmd := exec.CommandContext(ctx, "ip", "netns", "exec", a.Cfg.Namespace,
		selfExe, "_ns-daemon",
		"--install-dir", a.Cfg.InstallDir,
		"--state-dir", a.Cfg.StateDir,
		"--web-port", strconv.Itoa(a.Cfg.WebPort),
	)
	nsDaemonCmd.Stdout = os.Stdout
	nsDaemonCmd.Stderr = os.Stderr
	nsDaemonCmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS,
	}
	if err := nsDaemonCmd.Start(); err != nil {
		return fmt.Errorf("start namespace leigod daemon: %w", err)
	}
	go func() {
		if err := nsDaemonCmd.Wait(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("namespace leigod daemon exited: %w", err)
		}
	}()

	a.logf("Leigod isolated service ready:")
	a.logf("  • Physical WAN bypass : %s (gw %s, LAN IP %s, table %d)", wan.Interface, wan.Gateway, wan.LocalIP, a.Cfg.RouteTable)
	a.logf("  • Mobile App Discovery: UDP :%d & WebSocket :%d -> %s", a.Cfg.DiscoveryPort, a.Cfg.WebPort, a.Cfg.Namespace)
	a.logf("  • Throne/Sing-box SOCKS5 (TCP+UDP): %s (or direct %s)", a.Cfg.Socks5Listen, a.Cfg.NSSocks5Listen)
	a.logf("  • Per-process launcher: leigod run -- <command>")

	// Periodically back up accelerator.ini binding token
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return err
		case <-ticker.C:
			assets.BackupBindingState(a.Cfg.InstallDir, a.Cfg.StateDir)
		}
	}
}

// RunNSSocks5 runs inside the isolated network namespace as nobody (UID 65534)
// so all outbound TCP/UDP connections match Leigod's `! --uid-owner root` iptables rule.
func (a *App) RunNSSocks5(ctx context.Context) error {
	nsIPStr, err := a.Cfg.NSVethIP()
	if err != nil {
		return err
	}
	announceIP := net.ParseIP(nsIPStr)

	// Drop privileges to nobody (65534:65534) before creating sockets
	if os.Geteuid() == 0 {
		_ = syscall.Setgroups([]int{nobodyGID})
		if err := syscall.Setresgid(nobodyGID, nobodyGID, nobodyGID); err != nil {
			return fmt.Errorf("drop gid to nobody: %w", err)
		}
		if err := syscall.Setresuid(nobodyUID, nobodyUID, nobodyUID); err != nil {
			return fmt.Errorf("drop uid to nobody: %w", err)
		}
	}

	srv := &socks5.Server{
		ListenAddr: a.Cfg.NSSocks5Listen,
		AnnounceIP: announceIP,
		Logf:       a.logf,
	}
	// Also forward 127.0.0.1:57343 inside leigod-ns to host veth 10.254.254.1:57343 for Steam API IPC
	if hostIP, err := a.Cfg.HostVethIP(); err == nil {
		steamNSBridge := &bridge.TCPProxy{
			ListenAddr: "127.0.0.1:57343",
			TargetAddr: net.JoinHostPort(hostIP, "57343"),
			Logf:       a.logf,
		}
		go func() {
			_ = steamNSBridge.Start(ctx)
		}()
	}
	a.logf("namespace SOCKS5 server listening on %s (uid=%d, gid=%d)", a.Cfg.NSSocks5Listen, os.Getuid(), os.Getgid())
	return srv.Start(ctx)
}

// RunNSDaemon runs inside the isolated network namespace AND a private mount namespace.
// It constructs the SteamDeck filesystem view without polluting the host filesystem.
func (a *App) RunNSDaemon(ctx context.Context) error {
	paths := assets.ResolvePaths(a.Cfg.InstallDir)

	// 1. Make all mounts in this namespace slave/private so nothing leaks to host
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("set mount propagation slave: %w", err)
	}

	// 2. Bind-mount SteamDeck identity files
	if err := bindMountFile(paths.FakeProductName, "/sys/class/dmi/id/product_name"); err != nil {
		a.logf("warning: bind mount product_name: %v", err)
	}
	for _, osRel := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if _, err := os.Stat(osRel); err == nil {
			_ = bindMountFile(paths.FakeOSRelease, osRel)
		}
	}
	if _, err := os.Stat(paths.ResolvConf); err == nil {
		_ = bindMountFile(paths.ResolvConf, "/etc/resolv.conf")
	}

	// 3. Construct isolated /home/deck/leigod and /home/leigod inside a private tmpfs on /home
	if err := syscall.Mount("tmpfs", "/home", "tmpfs", 0, "mode=0755,size=16m"); err != nil {
		return fmt.Errorf("mount tmpfs on /home: %w", err)
	}
	for _, target := range []string{"/home/deck/leigod", "/home/leigod"} {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", target, err)
		}
		if err := syscall.Mount(paths.RuntimeDir, target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind mount %s -> %s: %w", paths.RuntimeDir, target, err)
		}
	}

	// 4. Prepare /tmp/acc/log mapped to paths.LogDir
	if err := os.MkdirAll("/tmp/acc/log", 0o755); err != nil {
		return fmt.Errorf("mkdir /tmp/acc/log: %w", err)
	}
	_ = syscall.Mount(paths.LogDir, "/tmp/acc/log", "", syscall.MS_BIND, "")

	if err := os.Chdir("/home/deck/leigod"); err != nil {
		return fmt.Errorf("chdir /home/deck/leigod: %w", err)
	}
	_ = os.Setenv("HOME", "/home/deck")

	// Clean up any stale lock files left by previous runs
	_ = os.Remove("/var/run/acc_daemon.lock")
	_ = os.Remove("/tmp/acc/upgrade_flag")

	defer killAccProcesses()

	accBin := "/home/deck/leigod/acc-gw.router.amd64"
	portStr := strconv.Itoa(a.Cfg.WebPort)

	a.logf("SteamDeck sandbox initialized; supervising %s", accBin)

	for {
		if !isAccDaemonRunning() {
			a.logf("starting acc-gw.router.amd64 -r daemon -m tun -p %s", portStr)
			cmd := exec.Command(accBin, "-r", "daemon", "-m", "tun", "-p", portStr)
			cmd.Dir = "/home/deck/leigod"
			if err := cmd.Start(); err != nil {
				a.logf("failed to start acc-gw.router.amd64: %v", err)
			} else {
				// Reap child process asynchronously in case it daemonizes
				go func() { _ = cmd.Wait() }()
			}
		}

		truncateOversizedLogs("/tmp/acc/log")

		select {
		case <-ctx.Done():
			a.logf("stopping Leigod daemon inside namespace...")
			killAccProcesses()
			return nil
		case <-time.After(4 * time.Second):
		}
	}
}

func bindMountFile(src, dst string) error {
	// Resolve symlink on dst if needed (e.g. /etc/os-release -> /usr/lib/os-release)
	realDst := dst
	if resolved, err := filepath.EvalSymlinks(dst); err == nil {
		realDst = resolved
	}
	return syscall.Mount(src, realDst, "", syscall.MS_BIND, "")
}

func isAccDaemonRunning() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if len(pid) == 0 || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil {
			continue
		}
		s := string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '}))
		if strings.Contains(s, "acc-gw.router.amd64") && strings.Contains(s, "-r daemon") {
			return true
		}
	}
	return false
}

func killAccProcesses() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		s := string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '}))
		if strings.Contains(s, "acc-gw.router") || strings.Contains(s, "acc_upgrade_monitor") {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if len(pids) > 0 {
		time.Sleep(500 * time.Millisecond)
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func truncateOversizedLogs(logDir string) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		p := filepath.Join(logDir, e.Name())
		if info, err := os.Stat(p); err == nil && info.Size() > maxDaemonLogSize {
			_ = os.Truncate(p, 0)
		}
	}
}
