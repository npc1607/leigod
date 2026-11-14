package netns

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"leigod/internal/config"
)

const (
	natChainName = "LEIGOD_NAT"
	fwdChainName = "LEIGOD_FWD"
)

// WANInfo holds the detected physical network interface, gateway, and local LAN IP.
type WANInfo struct {
	Interface string
	Gateway   string
	LocalIP   string
}

// CommandRunner abstracts command execution for testing.
type CommandRunner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) (string, error)
}

type osRunner struct{}

func (osRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (osRunner) Output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Manager manages the isolated network namespace, veth pair, policy routing, and firewall chains.
type Manager struct {
	Cfg    config.Config
	Runner CommandRunner
	Logf   func(string, ...any)
}

// NewManager creates a Manager with the default OS command runner.
func NewManager(cfg config.Config, logf func(string, ...any)) *Manager {
	return &Manager{
		Cfg:    cfg,
		Runner: osRunner{},
		Logf:   logf,
	}
}

// IsVirtualOrProxyIface returns true if the interface name belongs to a TUN/VPN/container interface
// (such as Throne/Sing-box tun, Clash Meta, dae, WireGuard, Docker, or our own veth).
func IsVirtualOrProxyIface(iface string) bool {
	iface = strings.ToLower(strings.TrimSpace(iface))
	if iface == "" || iface == "lo" {
		return true
	}
	prefixes := []string{
		"tun", "tap", "utun", "nekobox", "throne", "sing", "clash", "mihomo",
		"meta", "dae", "wg", "tailscale", "ts", "docker", "br-", "veth",
		"virbr", "leigod", "uu", "wlan0", "dummy", "zt", "vmnet",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(iface, p) {
			return true
		}
	}
	return false
}

// ParseMainRouteTable parses `ip -4 route show table main` output to find the physical WAN
// interface, gateway, and LAN source IP, ignoring Throne/Sing-box TUN routes.
func ParseMainRouteTable(routeOutput, configuredWAN, configuredIP string) (WANInfo, error) {
	wantIface := strings.TrimSpace(configuredWAN)
	if wantIface == "auto" {
		wantIface = ""
	}
	wantIP := strings.TrimSpace(configuredIP)
	if wantIP == "auto" || wantIP == "none" {
		wantIP = ""
	}

	lines := strings.Split(routeOutput, "\n")
	var info WANInfo

	// Pass 1: find default route on physical interface
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "default" {
			continue
		}
		var via, dev, src string
		for i := 1; i < len(fields)-1; i++ {
			switch fields[i] {
			case "via":
				via = fields[i+1]
			case "dev":
				dev = fields[i+1]
			case "src":
				src = fields[i+1]
			}
		}
		if dev == "" || via == "" {
			continue
		}
		if wantIface != "" && dev != wantIface {
			continue
		}
		if wantIface == "" && IsVirtualOrProxyIface(dev) {
			continue
		}
		info.Interface = dev
		info.Gateway = via
		info.LocalIP = src
		break
	}

	if info.Interface == "" {
		return WANInfo{}, fmt.Errorf("could not detect physical WAN default route from main route table")
	}

	// Pass 2: if src IP was not on the default route line, look for scope link route on the same interface
	if info.LocalIP == "" {
		for _, line := range lines {
			fields := strings.Fields(line)
			var dev, src string
			for i := 0; i < len(fields)-1; i++ {
				switch fields[i] {
				case "dev":
					dev = fields[i+1]
				case "src":
					src = fields[i+1]
				}
			}
			if dev == info.Interface && src != "" {
				info.LocalIP = src
				break
			}
		}
	}

	if wantIP != "" {
		info.LocalIP = wantIP
	}
	return info, nil
}

// DetectWAN inspects the host routing table and network interfaces to determine WANInfo.
func (m *Manager) DetectWAN() (WANInfo, error) {
	out, err := m.Runner.Output("ip", "-4", "route", "show", "table", "main")
	if err != nil {
		return WANInfo{}, fmt.Errorf("query main route table: %w", err)
	}
	info, err := ParseMainRouteTable(out, m.Cfg.WANInterface, m.Cfg.DeviceIP)
	if err != nil {
		return WANInfo{}, err
	}
	if info.LocalIP == "" {
		if iface, err := net.InterfaceByName(info.Interface); err == nil {
			addrs, _ := iface.Addrs()
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					info.LocalIP = ipNet.IP.To4().String()
					break
				}
			}
		}
	}
	if info.LocalIP == "" {
		return WANInfo{}, fmt.Errorf("could not determine IPv4 address for interface %s", info.Interface)
	}
	return info, nil
}

// Setup creates and configures the isolated network namespace, veth pair, dummy wlan0,
// bypass policy routing table, and dedicated iptables chains.
func (m *Manager) Setup(deviceMAC string) (WANInfo, error) {
	wan, err := m.DetectWAN()
	if err != nil {
		return WANInfo{}, err
	}
	if m.Logf != nil {
		m.Logf("detected physical WAN: dev=%s gw=%s ip=%s", wan.Interface, wan.Gateway, wan.LocalIP)
	}

	hostIP, err := m.Cfg.HostVethIP()
	if err != nil {
		return WANInfo{}, err
	}
	subnet, err := m.Cfg.VethSubnet()
	if err != nil {
		return WANInfo{}, err
	}

	// Clean up stale veth/rules before recreating
	_ = m.CleanupHostRules(wan.Interface)
	_ = m.Runner.Run("ip", "link", "delete", m.Cfg.VethHost)

	// 1. Ensure netns exists
	if !m.NamespaceExists() {
		if err := m.Runner.Run("ip", "netns", "add", m.Cfg.Namespace); err != nil {
			return WANInfo{}, fmt.Errorf("create netns %s: %w", m.Cfg.Namespace, err)
		}
	}

	// 2. Bring up loopback in netns
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "set", "lo", "up"); err != nil {
		return WANInfo{}, fmt.Errorf("bring up netns lo: %w", err)
	}

	// 3. Create veth pair and move peer into netns
	_ = m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "delete", m.Cfg.VethNS)
	if err := m.Runner.Run("ip", "link", "add", m.Cfg.VethHost, "type", "veth", "peer", "name", m.Cfg.VethNS); err != nil {
		return WANInfo{}, fmt.Errorf("create veth pair: %w", err)
	}
	if err := m.Runner.Run("ip", "link", "set", m.Cfg.VethNS, "netns", m.Cfg.Namespace); err != nil {
		_ = m.Runner.Run("ip", "link", "delete", m.Cfg.VethHost)
		return WANInfo{}, fmt.Errorf("move veth peer to netns: %w", err)
	}

	// 4. Configure addresses and default route inside netns
	if err := m.Runner.Run("ip", "addr", "add", m.Cfg.HostVethCIDR, "dev", m.Cfg.VethHost); err != nil {
		return WANInfo{}, fmt.Errorf("assign host veth IP: %w", err)
	}
	if err := m.Runner.Run("ip", "link", "set", m.Cfg.VethHost, "up"); err != nil {
		return WANInfo{}, fmt.Errorf("bring up host veth: %w", err)
	}
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "addr", "add", m.Cfg.NSVethCIDR, "dev", m.Cfg.VethNS); err != nil {
		return WANInfo{}, fmt.Errorf("assign netns veth IP: %w", err)
	}
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "set", m.Cfg.VethNS, "up"); err != nil {
		return WANInfo{}, fmt.Errorf("bring up netns veth: %w", err)
	}
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "route", "replace", "default", "via", hostIP, "dev", m.Cfg.VethNS); err != nil {
		return WANInfo{}, fmt.Errorf("set netns default route: %w", err)
	}

	// 5. Create dummy wlan0 inside netns with persistent MAC and host LAN IP (/32) for Leigod SteamDeck identity
	if err := m.setupNetnsWlan0(deviceMAC, wan.LocalIP); err != nil {
		return WANInfo{}, err
	}

	// 5b. Configure DNS fallback inside netns so apps reading host 127.0.0.53/127.0.0.1:53 work seamlessly
	if err := m.setupNetnsDNS(); err != nil {
		return WANInfo{}, err
	}

	// 6. Enable IPv4 forwarding and loose rp_filter for policy routing
	_ = writeSysctl("/proc/sys/net/ipv4/ip_forward", "1")
	_ = writeSysctl("/proc/sys/net/ipv4/conf/all/rp_filter", "2")
	_ = writeSysctl("/proc/sys/net/ipv4/conf/"+m.Cfg.VethHost+"/rp_filter", "0")
	_ = writeSysctl("/proc/sys/net/ipv4/conf/"+wan.Interface+"/rp_filter", "2")

	// 7. Configure host policy routing table so traffic from/to leigod-ns bypasses Throne/Sing-box TUN
	tableStr := strconv.Itoa(m.Cfg.RouteTable)
	prefOut := strconv.Itoa(m.Cfg.RulePref)
	prefIn := strconv.Itoa(m.Cfg.RulePref - 1)

	if err := m.Runner.Run("ip", "-4", "route", "replace", subnet, "dev", m.Cfg.VethHost, "src", hostIP, "table", tableStr); err != nil {
		return WANInfo{}, fmt.Errorf("add veth subnet to table %s: %w", tableStr, err)
	}
	if err := m.Runner.Run("ip", "-4", "route", "replace", "default", "via", wan.Gateway, "dev", wan.Interface, "src", wan.LocalIP, "table", tableStr); err != nil {
		return WANInfo{}, fmt.Errorf("add physical default route to table %s: %w", tableStr, err)
	}
	if err := m.Runner.Run("ip", "-4", "rule", "add", "from", subnet, "pref", prefOut, "lookup", tableStr); err != nil {
		return WANInfo{}, fmt.Errorf("add ip rule from %s: %w", subnet, err)
	}
	if err := m.Runner.Run("ip", "-4", "rule", "add", "to", subnet, "pref", prefIn, "lookup", tableStr); err != nil {
		return WANInfo{}, fmt.Errorf("add ip rule to %s: %w", subnet, err)
	}

	// 8. Configure isolated iptables chains for NAT and forwarding
	if err := m.setupIPTables(subnet, wan.Interface); err != nil {
		return WANInfo{}, err
	}

	return wan, nil
}

func (m *Manager) setupNetnsWlan0(deviceMAC, lanIP string) error {
	_ = m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "delete", "wlan0")
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "add", "wlan0", "type", "dummy"); err != nil {
		return fmt.Errorf("create dummy wlan0 in netns (ensure dummy kernel module is loaded): %w", err)
	}
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "set", "wlan0", "address", deviceMAC); err != nil {
		return fmt.Errorf("set wlan0 MAC %s: %w", deviceMAC, err)
	}
	if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "link", "set", "wlan0", "up"); err != nil {
		return fmt.Errorf("bring up wlan0 in netns: %w", err)
	}
	if lanIP != "" && m.Cfg.DeviceIP != "none" {
		if err := m.Runner.Run("ip", "-n", m.Cfg.Namespace, "addr", "add", lanIP+"/32", "dev", "wlan0"); err != nil {
			return fmt.Errorf("assign LAN IP %s/32 to netns wlan0: %w", lanIP, err)
		}
	}
	return nil
}

func (m *Manager) setupNetnsDNS() error {
	primaryDNS := "223.5.5.5"
	if len(m.Cfg.DNSServers) > 0 && strings.TrimSpace(m.Cfg.DNSServers[0]) != "" {
		primaryDNS = strings.TrimSpace(m.Cfg.DNSServers[0])
	}
	target := primaryDNS + ":53"

	// Write /etc/netns/<namespace>/resolv.conf for `ip netns exec` compatibility
	etcNetnsDir := "/etc/netns/" + m.Cfg.Namespace
	if err := os.MkdirAll(etcNetnsDir, 0o755); err == nil {
		var b strings.Builder
		for _, s := range m.Cfg.DNSServers {
			s = strings.TrimSpace(s)
			if s != "" {
				b.WriteString("nameserver " + s + "\n")
			}
		}
		if b.Len() == 0 {
			b.WriteString("nameserver 223.5.5.5\nnameserver 119.29.29.29\n")
		}
		_ = os.WriteFile(etcNetnsDir+"/resolv.conf", []byte(b.String()), 0o644)
	}

	// Redirect 127.0.0.53:53 and 127.0.0.1:53 inside netns to primaryDNS:53 so games using
	// systemd-resolved stub still resolve DNS and enter Leigod's TUN DNS interceptor.
	for _, proto := range []string{"udp", "tcp"} {
		for _, stub := range []string{"127.0.0.53", "127.0.0.1"} {
			_ = m.Runner.Run("ip", "netns", "exec", m.Cfg.Namespace,
				"iptables", "-t", "nat", "-A", "OUTPUT",
				"-d", stub, "-p", proto, "--dport", "53",
				"-j", "DNAT", "--to-destination", target)
		}
	}
	return nil
}

func (m *Manager) setupIPTables(subnet, wanIface string) error {
	// Dedicated NAT chain
	_ = m.Runner.Run("iptables", "-t", "nat", "-N", natChainName)
	_ = m.Runner.Run("iptables", "-t", "nat", "-F", natChainName)
	if err := m.Runner.Run("iptables", "-t", "nat", "-A", natChainName, "-s", subnet, "-o", wanIface, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("add MASQUERADE rule: %w", err)
	}
	if m.Runner.Run("iptables", "-t", "nat", "-C", "POSTROUTING", "-j", natChainName) != nil {
		if err := m.Runner.Run("iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-j", natChainName); err != nil {
			return fmt.Errorf("hook %s into POSTROUTING: %w", natChainName, err)
		}
	}

	// Dedicated FORWARD chain
	_ = m.Runner.Run("iptables", "-t", "filter", "-N", fwdChainName)
	_ = m.Runner.Run("iptables", "-t", "filter", "-F", fwdChainName)
	if err := m.Runner.Run("iptables", "-t", "filter", "-A", fwdChainName, "-i", m.Cfg.VethHost, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("add forward in rule: %w", err)
	}
	if err := m.Runner.Run("iptables", "-t", "filter", "-A", fwdChainName, "-o", m.Cfg.VethHost, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("add forward out rule: %w", err)
	}
	if m.Runner.Run("iptables", "-t", "filter", "-C", "FORWARD", "-j", fwdChainName) != nil {
		if err := m.Runner.Run("iptables", "-t", "filter", "-I", "FORWARD", "1", "-j", fwdChainName); err != nil {
			return fmt.Errorf("hook %s into FORWARD: %w", fwdChainName, err)
		}
	}
	return nil
}

// CleanupHostRules removes our custom iptables chains and policy routing rules without touching any other host rules.
func (m *Manager) CleanupHostRules(wanIface string) error {
	tableStr := strconv.Itoa(m.Cfg.RouteTable)
	prefOut := strconv.Itoa(m.Cfg.RulePref)
	prefIn := strconv.Itoa(m.Cfg.RulePref - 1)
	subnet, _ := m.Cfg.VethSubnet()

	if subnet != "" {
		for i := 0; i < 3; i++ {
			_ = m.Runner.Run("ip", "-4", "rule", "del", "from", subnet, "pref", prefOut, "lookup", tableStr)
			_ = m.Runner.Run("ip", "-4", "rule", "del", "to", subnet, "pref", prefIn, "lookup", tableStr)
		}
	}
	_ = m.Runner.Run("ip", "-4", "route", "flush", "table", tableStr)

	// Unhook and delete custom NAT chain
	for i := 0; i < 3; i++ {
		if m.Runner.Run("iptables", "-t", "nat", "-D", "POSTROUTING", "-j", natChainName) != nil {
			break
		}
	}
	_ = m.Runner.Run("iptables", "-t", "nat", "-F", natChainName)
	_ = m.Runner.Run("iptables", "-t", "nat", "-X", natChainName)

	// Unhook and delete custom FORWARD chain
	for i := 0; i < 3; i++ {
		if m.Runner.Run("iptables", "-t", "filter", "-D", "FORWARD", "-j", fwdChainName) != nil {
			break
		}
	}
	_ = m.Runner.Run("iptables", "-t", "filter", "-F", fwdChainName)
	_ = m.Runner.Run("iptables", "-t", "filter", "-X", fwdChainName)

	return nil
}

// Teardown destroys the veth pair, cleans up host policy routes/iptables, and deletes the network namespace.
func (m *Manager) Teardown() error {
	_ = m.CleanupHostRules("")
	_ = m.Runner.Run("ip", "link", "delete", m.Cfg.VethHost)
	if m.NamespaceExists() {
		// Kill any remaining processes inside the namespace before deleting it
		if out, err := m.Runner.Output("ip", "netns", "pids", m.Cfg.Namespace); err == nil {
			for _, pid := range strings.Fields(out) {
				_ = m.Runner.Run("kill", "-TERM", pid)
			}
		}
		_ = m.Runner.Run("ip", "netns", "delete", m.Cfg.Namespace)
	}
	_ = os.RemoveAll("/etc/netns/" + m.Cfg.Namespace)
	return nil
}

// NamespaceExists checks if /run/netns/<Namespace> exists.
func (m *Manager) NamespaceExists() bool {
	_, err := os.Stat("/run/netns/" + m.Cfg.Namespace)
	return err == nil
}

// AccTunActive checks if Leigod has created its acceleration tun device inside the namespace.
func (m *Manager) AccTunActive() (string, bool) {
	if !m.NamespaceExists() {
		return "", false
	}
	out, err := m.Runner.Output("ip", "-n", m.Cfg.Namespace, "-o", "link", "show")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			name := strings.TrimSuffix(fields[1], ":")
			if idx := strings.IndexByte(name, '@'); idx >= 0 {
				name = name[:idx]
			}
			if strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "acc") {
				return name, true
			}
		}
	}
	// Fallback for non-root `leigod status`: check if `acc-gw.router.amd64 -r acc` process is running
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if len(pid) == 0 || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			continue
		}
		s := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if strings.Contains(s, "acc-gw.router") && strings.Contains(s, "-r acc") {
			return "tun_PC", true
		}
	}
	return "", false
}

func writeSysctl(path, val string) error {
	return os.WriteFile(path, []byte(val+"\n"), 0o644)
}
