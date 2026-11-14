package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime and installation settings for the isolated Leigod service.
type Config struct {
	InstallDir     string
	StateDir       string
	Namespace      string
	VethHost       string
	VethNS         string
	HostVethCIDR   string
	NSVethCIDR     string
	RouteTable     int
	RulePref       int
	WANInterface   string
	DeviceMAC      string
	DeviceIP       string
	MirrorURL      string
	WebPort        int
	DiscoveryPort  int
	Socks5Listen   string
	NSSocks5Listen string
	WebListen      string
	DNSServers     []string
	SkipVerify     bool
	Purge          bool
	// WhitelistMode 控制 IP 白名单下载（dfs01.nn.com）的处理方式：
	//   off   = 不干预（默认，正常工作）
	//   block = 解析到 0.0.0.0（会让 acc 引擎初始化失败，仅用于诊断）
	//   serve = 本地 HTTPS 提供自定义白名单内容（WhitelistBody，默认空 → 全量加速）
	WhitelistMode string
	// WhitelistBody 是 serve 模式下返回的文件内容；空字符串表示 0 字节（触发
	// "ip whitelist size is zero -> go acc"）。
	WhitelistBody string
	// AccModeOverride >= 0 时，把手机下发的 "acc_mode":N 改写成该值。
	// 雷神内部只接受 0/1；当前观察到 1 = 智能（只加速白名单），0 疑似全量。
	AccModeOverride int
}

// Default returns a Config populated with safe defaults for desktop Linux + Throne/Sing-box coexistence.
func Default() Config {
	return Config{
		InstallDir:      "/opt/leigod",
		StateDir:        "/var/lib/leigod",
		Namespace:       "leigod-ns",
		VethHost:        "leigod-host",
		VethNS:          "eth0",
		HostVethCIDR:    "10.254.254.1/24",
		NSVethCIDR:      "10.254.254.2/24",
		RouteTable:      2540,
		RulePref:        90,
		WANInterface:    "auto",
		DeviceMAC:       "auto",
		DeviceIP:        "auto",
		MirrorURL:       "http://119.3.40.126",
		WebPort:         5588,
		DiscoveryPort:   6066,
		Socks5Listen:    "127.0.0.1:15588",
		NSSocks5Listen:  "10.254.254.2:1080",
		WebListen:       "127.0.0.1:7788",
		DNSServers:      []string{"223.5.5.5", "119.29.29.29"},
		SkipVerify:      false,
		AccModeOverride: -1,
	}
}

// HostVethIP returns the IP part of HostVethCIDR without prefix length.
func (c Config) HostVethIP() (string, error) {
	ip, _, err := net.ParseCIDR(c.HostVethCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid host_veth_cidr %q: %w", c.HostVethCIDR, err)
	}
	return ip.String(), nil
}

// NSVethIP returns the IP part of NSVethCIDR without prefix length.
func (c Config) NSVethIP() (string, error) {
	ip, _, err := net.ParseCIDR(c.NSVethCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid ns_veth_cidr %q: %w", c.NSVethCIDR, err)
	}
	return ip.String(), nil
}

// VethSubnet returns the network CIDR (e.g. 10.254.254.0/24) for NAT rules.
func (c Config) VethSubnet() (string, error) {
	_, ipNet, err := net.ParseCIDR(c.HostVethCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid host_veth_cidr %q: %w", c.HostVethCIDR, err)
	}
	return ipNet.String(), nil
}

// Validate checks that the configuration values are well-formed.
func (c Config) Validate() error {
	if strings.TrimSpace(c.InstallDir) == "" {
		return fmt.Errorf("install_dir cannot be empty")
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return fmt.Errorf("state_dir cannot be empty")
	}
	if strings.TrimSpace(c.Namespace) == "" {
		return fmt.Errorf("namespace cannot be empty")
	}
	if len(c.VethHost) == 0 || len(c.VethHost) > 15 {
		return fmt.Errorf("veth_host %q must be 1..15 characters", c.VethHost)
	}
	if len(c.VethNS) == 0 || len(c.VethNS) > 15 {
		return fmt.Errorf("veth_ns %q must be 1..15 characters", c.VethNS)
	}
	if _, err := c.HostVethIP(); err != nil {
		return err
	}
	if _, err := c.NSVethIP(); err != nil {
		return err
	}
	if c.RouteTable <= 0 || c.RouteTable > 32765 {
		return fmt.Errorf("route_table must be between 1 and 32765")
	}
	if c.RulePref <= 0 || c.RulePref > 32765 {
		return fmt.Errorf("rule_pref must be between 1 and 32765")
	}
	if c.WebPort <= 0 || c.WebPort > 65535 {
		return fmt.Errorf("web_port must be between 1 and 65535")
	}
	if c.DiscoveryPort <= 0 || c.DiscoveryPort > 65535 {
		return fmt.Errorf("discovery_port must be between 1 and 65535")
	}
	return nil
}

// LoadYAMLFile parses a simple flat YAML file into the provided Config.
func LoadYAMLFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("%s:%d: invalid YAML line %q", path, lineNo, line)
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)

		switch key {
		case "install_dir":
			cfg.InstallDir = val
		case "state_dir":
			cfg.StateDir = val
		case "namespace":
			cfg.Namespace = val
		case "veth_host":
			cfg.VethHost = val
		case "veth_ns":
			cfg.VethNS = val
		case "host_veth_cidr":
			cfg.HostVethCIDR = val
		case "ns_veth_cidr":
			cfg.NSVethCIDR = val
		case "route_table":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid route_table %q: %w", path, lineNo, val, err)
			}
			cfg.RouteTable = v
		case "rule_pref":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid rule_pref %q: %w", path, lineNo, val, err)
			}
			cfg.RulePref = v
		case "wan_interface":
			cfg.WANInterface = val
		case "device_mac":
			cfg.DeviceMAC = val
		case "device_ip":
			cfg.DeviceIP = val
		case "mirror_url":
			cfg.MirrorURL = strings.TrimRight(val, "/")
		case "web_port":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid web_port %q: %w", path, lineNo, val, err)
			}
			cfg.WebPort = v
		case "discovery_port":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid discovery_port %q: %w", path, lineNo, val, err)
			}
			cfg.DiscoveryPort = v
		case "socks5_listen":
			cfg.Socks5Listen = val
		case "ns_socks5_listen":
			cfg.NSSocks5Listen = val
		case "web_listen":
			cfg.WebListen = val
		case "dns_servers":
			fields := strings.Split(val, ",")
			var servers []string
			for _, s := range fields {
				s = strings.TrimSpace(strings.Trim(s, `[]" `))
				if s != "" {
					servers = append(servers, s)
				}
			}
			if len(servers) > 0 {
				cfg.DNSServers = servers
			}
		case "whitelist_mode":
			cfg.WhitelistMode = val
		case "whitelist_body":
			cfg.WhitelistBody = val
		case "acc_mode_override":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid acc_mode_override %q: %w", path, lineNo, val, err)
			}
			cfg.AccModeOverride = v
		case "skip_verify":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid skip_verify %q: %w", path, lineNo, val, err)
			}
			cfg.SkipVerify = b
		default:
			return fmt.Errorf("%s:%d: unknown configuration key %q", path, lineNo, key)
		}
	}
	return scanner.Err()
}
