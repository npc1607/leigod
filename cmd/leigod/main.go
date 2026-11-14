package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"leigod/internal/app"
	"leigod/internal/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Split at "--" first so `leigod run -- %command% -arg1 -arg2` never parses game flags
	var beforeDash, afterDash []string
	dashIdx := -1
	for i, a := range args {
		if a == "--" {
			dashIdx = i
			break
		}
	}
	if dashIdx >= 0 {
		beforeDash = args[:dashIdx]
		afterDash = args[dashIdx+1:]
	} else {
		beforeDash = args
	}

	// Extract command verb and flags cleanly regardless of order (e.g. `leigod install --skip-verify` or `leigod --config x install`)
	var subcmd string
	var flagArgs []string
	var positional []string

	valueFlags := map[string]bool{
		"-config": true, "--config": true,
		"-install-dir": true, "--install-dir": true,
		"-state-dir": true, "--state-dir": true,
		"-namespace": true, "--namespace": true,
		"-wan": true, "--wan": true,
		"-mac": true, "--mac": true,
		"-ip": true, "--ip": true,
		"-mirror": true, "--mirror": true,
		"-socks5-listen": true, "--socks5-listen": true,
		"-ns-socks5-listen": true, "--ns-socks5-listen": true,
		"-listen": true, "--listen": true,
		"-ns-veth-cidr": true, "--ns-veth-cidr": true,
		"-web-port": true, "--web-port": true,
	}

	for i := 0; i < len(beforeDash); i++ {
		tok := beforeDash[i]
		if subcmd == "run" {
			positional = append(positional, tok)
			continue
		}
		if len(tok) > 0 && tok[0] == '-' {
			flagArgs = append(flagArgs, tok)
			if valueFlags[tok] && i+1 < len(beforeDash) {
				i++
				flagArgs = append(flagArgs, beforeDash[i])
			}
			continue
		}
		if subcmd == "" {
			subcmd = tok
		} else {
			positional = append(positional, tok)
		}
	}

	cfg := config.Default()
	// Automatically load /etc/leigod/config.yaml if it exists
	if _, err := os.Stat(app.DefaultYAML); err == nil {
		_ = config.LoadYAMLFile(app.DefaultYAML, &cfg)
	}

	fs := flag.NewFlagSet("leigod", flag.ContinueOnError)
	var configFile string
	fs.StringVar(&configFile, "config", "", "Path to YAML configuration file")
	fs.StringVar(&cfg.InstallDir, "install-dir", cfg.InstallDir, "Installation directory")
	fs.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "Persistent state directory")
	fs.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "Network namespace name")
	fs.StringVar(&cfg.WANInterface, "wan", cfg.WANInterface, "Physical WAN interface (default: auto)")
	fs.StringVar(&cfg.DeviceMAC, "mac", cfg.DeviceMAC, "Device MAC/SN override (default: auto)")
	fs.StringVar(&cfg.DeviceIP, "ip", cfg.DeviceIP, "Device LAN IP override (default: auto)")
	fs.StringVar(&cfg.MirrorURL, "mirror", cfg.MirrorURL, "Upstream asset mirror URL")
	fs.StringVar(&cfg.Socks5Listen, "socks5-listen", cfg.Socks5Listen, "Host SOCKS5 listen address for Throne/Sing-box")
	fs.StringVar(&cfg.NSSocks5Listen, "ns-socks5-listen", cfg.NSSocks5Listen, "Namespace internal SOCKS5 listen address")
	fs.StringVar(&cfg.WebListen, "listen", cfg.WebListen, "Web dashboard listen address (default 127.0.0.1:7788)")
	fs.StringVar(&cfg.NSVethCIDR, "ns-veth-cidr", cfg.NSVethCIDR, "Namespace internal veth CIDR")
	fs.IntVar(&cfg.WebPort, "web-port", cfg.WebPort, "WebSocket control port")
	fs.BoolVar(&cfg.SkipVerify, "skip-verify", cfg.SkipVerify, "Allow upstream binary even if SHA-256 is newer than allowlist")
	fs.BoolVar(&cfg.Purge, "purge", cfg.Purge, "Remove persistent state (/var/lib/leigod) and config (/etc/leigod) during uninstall")

	fs.Usage = printUsage
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	if configFile != "" {
		if err := config.LoadYAMLFile(configFile, &cfg); err != nil {
			return err
		}
		// Re-parse CLI flags so explicit CLI flags override config file values
		_ = fs.Parse(flagArgs)
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	if subcmd == "" {
		subcmd = "status"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	instance := app.New(cfg)

	switch subcmd {
	case "install":
		return instance.Install(ctx)
	case "uninstall", "remove", "delete":
		return instance.Uninstall()
	case "start":
		return instance.Start()
	case "stop":
		return instance.Stop()
	case "restart":
		return instance.Restart()
	case "status":
		return instance.Status()
	case "web":
		return instance.RunWeb(ctx)
	case "daemon":
		return instance.RunDaemon(ctx)
	case "_ns-daemon":
		return instance.RunNSDaemon(ctx)
	case "_ns-socks5":
		return instance.RunNSSocks5(ctx)
	case "_steam-ipc-bridge":
		return instance.RunSteamIPCBridge(ctx)
	case "run":
		runCmd := append(positional, afterDash...)
		return instance.RunInNamespace(runCmd)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", subcmd)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `leigod - Isolated Leigod Accelerator for Linux (Network Namespace + Throne/Sing-box Integration)

Usage:
  sudo leigod install [--skip-verify]   Install assets, capability helper, and start systemd service
  sudo leigod uninstall [--purge]       Stop service, clean up namespace/rules, and remove files
  sudo leigod start | stop | restart    Manage leigod-isolated.service
  leigod status                         Show service, binding, namespace, and tunnel status
  leigod web [--listen addr]            Serve a read-only web dashboard (default 127.0.0.1:7788)
  leigod run -- <command> [args...]     Run a game/process inside the isolated Leigod namespace
  sudo leigod daemon                    Run supervisor daemon in foreground

Flags:
  --config <path>         Path to YAML config file (default: /etc/leigod/config.yaml)
  --install-dir <dir>     Installation directory (default: /opt/leigod)
  --namespace <name>      Network namespace name (default: leigod-ns)
  --wan <iface>           Physical WAN interface (default: auto)
  --socks5-listen <addr>  Host SOCKS5 listen address for Throne/Sing-box (default: 127.0.0.1:15588)
  --listen <addr>         Web dashboard listen address (default: 127.0.0.1:7788)
  --skip-verify           Accept upstream asset SHA-256 not yet in local allowlist
  --purge                 Also remove /var/lib/leigod and /etc/leigod on uninstall
`)
}
