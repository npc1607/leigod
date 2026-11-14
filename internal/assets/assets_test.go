package assets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveOrLoadDeviceMAC(t *testing.T) {
	dir := t.TempDir()
	midFile := filepath.Join(dir, "machine-id")
	if err := os.WriteFile(midFile, []byte("abcdef1234567890abcdef1234567890\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(dir, "state")
	mac1, err := DeriveOrLoadDeviceMAC("auto", stateDir, midFile)
	if err != nil {
		t.Fatalf("DeriveOrLoadDeviceMAC failed: %v", err)
	}
	if len(mac1) != 17 || mac1[:3] != "02:" {
		t.Fatalf("unexpected MAC format: %q", mac1)
	}

	// Changing machine-id after state is persisted should still return persisted mac1
	if err := os.WriteFile(midFile, []byte("different-machine-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mac2, err := DeriveOrLoadDeviceMAC("auto", stateDir, midFile)
	if err != nil || mac2 != mac1 {
		t.Fatalf("expected persisted MAC %q, got %q (%v)", mac1, mac2, err)
	}

	// Explicit override takes precedence
	mac3, err := DeriveOrLoadDeviceMAC("02:aa:bb:cc:dd:ee", stateDir, midFile)
	if err != nil || mac3 != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("expected override MAC, got %q (%v)", mac3, err)
	}
}

func TestHasBindingToken(t *testing.T) {
	if HasBindingToken(DefaultAcceleratorINI) {
		t.Fatal("DefaultAcceleratorINI should not have a binding token")
	}
	bound := DefaultAcceleratorINI + "\ntoken=abc123xyz\nuser_id=42\n"
	if !HasBindingToken(bound) {
		t.Fatal("expected HasBindingToken(bound) == true")
	}
}
