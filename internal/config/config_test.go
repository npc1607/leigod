package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() failed: %v", err)
	}
	hostIP, err := cfg.HostVethIP()
	if err != nil || hostIP != "10.254.254.1" {
		t.Fatalf("HostVethIP() = %q, %v", hostIP, err)
	}
	nsIP, err := cfg.NSVethIP()
	if err != nil || nsIP != "10.254.254.2" {
		t.Fatalf("NSVethIP() = %q, %v", nsIP, err)
	}
	subnet, err := cfg.VethSubnet()
	if err != nil || subnet != "10.254.254.0/24" {
		t.Fatalf("VethSubnet() = %q, %v", subnet, err)
	}
}

func TestLoadYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leigod.yaml")
	content := `
# Custom Leigod config
install_dir: /opt/leigod-custom
namespace: lg-ns
route_table: 2600
rule_pref: 85
wan_interface: enp34s0
socks5_listen: 127.0.0.1:10808
dns_servers: 223.5.5.5, 114.114.114.114
skip_verify: true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	if err := LoadYAMLFile(path, &cfg); err != nil {
		t.Fatalf("LoadYAMLFile failed: %v", err)
	}
	if cfg.InstallDir != "/opt/leigod-custom" {
		t.Errorf("InstallDir = %q", cfg.InstallDir)
	}
	if cfg.Namespace != "lg-ns" {
		t.Errorf("Namespace = %q", cfg.Namespace)
	}
	if cfg.RouteTable != 2600 || cfg.RulePref != 85 {
		t.Errorf("RouteTable=%d RulePref=%d", cfg.RouteTable, cfg.RulePref)
	}
	if cfg.WANInterface != "enp34s0" {
		t.Errorf("WANInterface = %q", cfg.WANInterface)
	}
	if cfg.Socks5Listen != "127.0.0.1:10808" {
		t.Errorf("Socks5Listen = %q", cfg.Socks5Listen)
	}
	if len(cfg.DNSServers) != 2 || cfg.DNSServers[1] != "114.114.114.114" {
		t.Errorf("DNSServers = %v", cfg.DNSServers)
	}
	if !cfg.SkipVerify {
		t.Errorf("SkipVerify = false, want true")
	}
}
