package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"leigod/internal/bridge"
)

const (
	sysSetns    = 308 // __NR_setns on x86_64 Linux
	sysCapset   = 126 // __NR_capset on x86_64 Linux
	prSetDumpable = 4 // PR_SET_DUMPABLE
	cloneNewNet = 0x40000000

	envSavedLDPreload     = "LEIGOD_SAVED_LD_PRELOAD"
	envSavedLDLibraryPath = "LEIGOD_SAVED_LD_LIBRARY_PATH"
	envStage2Marker       = "LEIGOD_NSENTER_STAGE2"

	linuxCapVersion3 = 0x20080522
)

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// RunInNamespace executes the given command and arguments inside the isolated Leigod
// network namespace as an unprivileged desktop user (UID != 0) so its traffic is
// intercepted by Leigod's TUN rules while keeping all Steam/desktop environment variables.
func (a *App) RunInNamespace(cmdArgs []string) error {
	if len(cmdArgs) == 0 {
		return fmt.Errorf("usage: leigod run -- <command> [args...]")
	}

	nsPath := "/run/netns/" + a.Cfg.Namespace
	if _, err := os.Stat(nsPath); err != nil {
		return fmt.Errorf("network namespace %q is not active (%v); start service first with: sudo leigod start", a.Cfg.Namespace, err)
	}

	// Restore any LD_PRELOAD / LD_LIBRARY_PATH saved before jumping through the cap_sys_admin helper
	restoreSavedLDEnv()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Keep a handle to the caller's (host) network namespace before switching into leigod-ns
	// so we can spawn an on-demand Steam IPC bridge (127.0.0.1:57343) if needed.
	hostNsFile, _ := os.Open("/proc/thread-self/ns/net")
	if hostNsFile == nil {
		hostNsFile, _ = os.Open("/proc/self/ns/net")
	}

	nsFile, err := os.Open(nsPath)
	if err != nil {
		if hostNsFile != nil {
			_ = hostNsFile.Close()
		}
		return fmt.Errorf("open %s: %w", nsPath, err)
	}
	_, _, errno := syscall.RawSyscall(sysSetns, nsFile.Fd(), uintptr(cloneNewNet), 0)

	if errno != 0 {
		_ = nsFile.Close()
		if hostNsFile != nil {
			_ = hostNsFile.Close()
		}
		// If we are running as an unprivileged user without CAP_SYS_ADMIN and have not yet
		// jumped through the cap_sys_admin helper binary, preserve LD_PRELOAD and re-exec via helper.
		if os.Getenv(envStage2Marker) == "" {
			if helperPath := a.findCapHelper(); helperPath != "" {
				return reexecViaCapHelper(helperPath, cmdArgs)
			}
		}
		return fmt.Errorf("setns(%s, CLONE_NEWNET) failed: %v (run with 'sudo -E leigod run -- ...' or install capability helper via 'sudo leigod install')", nsPath, errno)
	}

	// Ensure Steam IPC (127.0.0.1:57343) inside leigod-ns is bridged to the host's 127.0.0.1:57343
	ensureSteamIPCBridge(nsFile, hostNsFile)
	_ = nsFile.Close()
	if hostNsFile != nil {
		_ = hostNsFile.Close()
	}

	// We are now inside the isolated network namespace!
	// Ensure we run as a non-root user so Leigod's `! --uid-owner root` iptables rule accelerates the traffic.
	if os.Geteuid() == 0 {
		if err := dropRootToDesktopUser(); err != nil {
			return err
		}
	} else {
		// Clear any file capabilities acquired from leigod-nsenter before execing the game
		dropAllCapabilities()
	}

	// Reset PR_SET_DUMPABLE to 1 so /proc/self/uid_map is owned by the desktop user.
	// Without this, Steam's pressure-vessel (bubblewrap/bwrap) fails to set up uid_map for Proton games!
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 1, 0)

	binPath, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		return fmt.Errorf("command not found %q: %w", cmdArgs[0], err)
	}

	cleanEnv := filterInternalEnv(os.Environ())
	return syscall.Exec(binPath, cmdArgs, cleanEnv)
}

func (a *App) findCapHelper() string {
	candidates := []string{
		filepath.Join(a.Cfg.InstallDir, "bin", "leigod-nsenter"),
		"/opt/leigod/bin/leigod-nsenter",
	}
	if selfExe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(selfExe), "leigod-nsenter")}, candidates...)
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func reexecViaCapHelper(helperPath string, cmdArgs []string) error {
	if v := os.Getenv("LD_PRELOAD"); v != "" {
		_ = os.Setenv(envSavedLDPreload, v)
	}
	if v := os.Getenv("LD_LIBRARY_PATH"); v != "" {
		_ = os.Setenv(envSavedLDLibraryPath, v)
	}
	_ = os.Setenv(envStage2Marker, "1")

	argv := append([]string{helperPath, "--namespace", os.Getenv("LEIGOD_NAMESPACE"), "run", "--"}, cmdArgs...)
	if os.Getenv("LEIGOD_NAMESPACE") == "" {
		argv = append([]string{helperPath, "run", "--"}, cmdArgs...)
	}
	return syscall.Exec(helperPath, argv, os.Environ())
}

func restoreSavedLDEnv() {
	if v := os.Getenv(envSavedLDPreload); v != "" {
		_ = os.Setenv("LD_PRELOAD", v)
		_ = os.Unsetenv(envSavedLDPreload)
	}
	if v := os.Getenv(envSavedLDLibraryPath); v != "" {
		_ = os.Setenv("LD_LIBRARY_PATH", v)
		_ = os.Unsetenv(envSavedLDLibraryPath)
	}
	_ = os.Unsetenv(envStage2Marker)
}

func filterInternalEnv(env []string) []string {
	stripPrefixes := []string{
		envSavedLDPreload + "=",
		envSavedLDLibraryPath + "=",
		envStage2Marker + "=",
		"http_proxy=", "HTTP_PROXY=",
		"https_proxy=", "HTTPS_PROXY=",
		"all_proxy=", "ALL_PROXY=",
		"ftp_proxy=", "FTP_PROXY=",
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		skip := false
		for _, p := range stripPrefixes {
			if strings.HasPrefix(kv, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

func dropAllCapabilities() {
	hdr := capHeader{version: linuxCapVersion3, pid: 0}
	var data [2]capData
	_, _, _ = syscall.RawSyscall(
		sysCapset,
		uintptr(unsafe.Pointer(&hdr)),
		uintptr(unsafe.Pointer(&data[0])),
		0,
	)
}

func dropRootToDesktopUser() error {
	uid := nobodyUID
	gid := nobodyGID
	var username, homeDir string

	if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
		if v, err := strconv.Atoi(sudoUID); err == nil && v > 0 {
			uid = v
		}
	}
	if sudoGID := os.Getenv("SUDO_GID"); sudoGID != "" {
		if v, err := strconv.Atoi(sudoGID); err == nil && v > 0 {
			gid = v
		}
	}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		username = u.Username
		homeDir = u.HomeDir
		if gids, err := u.GroupIds(); err == nil && len(gids) > 0 {
			var groups []int
			for _, gStr := range gids {
				if g, err := strconv.Atoi(gStr); err == nil {
					groups = append(groups, g)
				}
			}
			if len(groups) > 0 {
				_ = syscall.Setgroups(groups)
			}
		}
	}

	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("drop gid to %d: %w", gid, err)
	}
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("drop uid to %d: %w", uid, err)
	}

	// Ensure desktop session environment variables point to the target user
	if username != "" {
		_ = os.Setenv("USER", username)
		_ = os.Setenv("LOGNAME", username)
	}
	if homeDir != "" {
		_ = os.Setenv("HOME", homeDir)
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" && uid != nobodyUID {
		runtimeDir := fmt.Sprintf("/run/user/%d", uid)
		if _, err := os.Stat(runtimeDir); err == nil {
			_ = os.Setenv("XDG_RUNTIME_DIR", runtimeDir)
		}
	}
	return nil
}

// ensureSteamIPCBridge guarantees that 127.0.0.1:57343 inside leigod-ns is listening and
// forwarding to 127.0.0.1:57343 in the host network namespace so SteamAPI_Init() succeeds.
func ensureSteamIPCBridge(nsFile, hostNsFile *os.File) {
	if hostNsFile == nil || nsFile == nil {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:57343")
	if err != nil {
		// Already bound by daemon or previous bridge process
		return
	}
	tcpLn, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return
	}
	lnFile, err := tcpLn.File()
	_ = ln.Close()
	if err != nil {
		return
	}
	defer lnFile.Close()

	selfExe, err := os.Executable()
	if err != nil {
		return
	}

	// Temporarily switch back to host network namespace to spawn the bridge process
	_, _, errno := syscall.RawSyscall(sysSetns, hostNsFile.Fd(), uintptr(cloneNewNet), 0)
	if errno == 0 {
		cmd := exec.Command(selfExe, "_steam-ipc-bridge")
		cmd.ExtraFiles = []*os.File{lnFile} // Passed as fd 3
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid: true,
		}
		_ = cmd.Start()
		// Switch back into leigod-ns
		_, _, _ = syscall.RawSyscall(sysSetns, nsFile.Fd(), uintptr(cloneNewNet), 0)
	}
}

// RunSteamIPCBridge runs in the host network namespace while accepting connections
// on fd 3 (which is bound to 127.0.0.1:57343 inside leigod-ns) and relaying them to
// the host's 127.0.0.1:57343 Steam client.
func (a *App) RunSteamIPCBridge(ctx context.Context) error {
	dropAllCapabilities()
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 1, 0)

	f := os.NewFile(3, "steam-ipc-listener")
	if f == nil {
		return fmt.Errorf("missing fd 3 listener")
	}
	defer f.Close()

	ln, err := net.FileListener(f)
	if err != nil {
		return fmt.Errorf("create file listener from fd 3: %w", err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil
		}
		go func(c net.Conn) {
			defer c.Close()
			dialer := net.Dialer{Timeout: 3 * time.Second}
			up, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:57343")
			if err != nil {
				return
			}
			defer up.Close()
			bridge.RelayTCP(c, up)
		}(conn)
	}
}
