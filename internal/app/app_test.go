package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leigod/internal/config"
)

func TestRenderDefaultYAMLRoundTrip(t *testing.T) {
	orig := config.Default()
	orig.WANInterface = "enp34s0"
	orig.Socks5Listen = "127.0.0.1:15588"

	yamlText := RenderDefaultYAML(orig)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded := config.Default()
	if err := config.LoadYAMLFile(p, &loaded); err != nil {
		t.Fatalf("LoadYAMLFile failed on rendered YAML: %v", err)
	}
	if loaded.WANInterface != "enp34s0" || loaded.RouteTable != orig.RouteTable {
		t.Fatalf("round-trip mismatch: %+v", loaded)
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	unit := RenderSystemdUnit("/opt/leigod/bin/leigod", "/etc/leigod/config.yaml")
	if !strings.Contains(unit, "ExecStart=/opt/leigod/bin/leigod --config /etc/leigod/config.yaml daemon") {
		t.Fatalf("unexpected unit content:\n%s", unit)
	}
}
