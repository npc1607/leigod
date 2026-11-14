package assets

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// KnownValidHashes lists verified upstream SHA-256 digests for Leigod official assets.
var KnownValidHashes = map[string]map[string]string{
	"acc-gw.router.amd64": {
		"3640d47abaee4a6b4312e3489137311ab309d27dcb92ab640c0fe3d953328c1b": "1.3.0.02",
		"f81ec6a7e1a5bcef8cb8d1bf233141ece7d54250881422ecc2814c98df819e23": "1.3.0.01",
		"826328a7b0b12e1d364ef5828114dc0bc6f18f2b1693b899f1802f7a0b31f6d1": "1.2.2.15",
	},
	"ipdatacloud_country.xdb": {
		"3536000ee9eb3997538cb37a14f90d571979734c48b23b8d548eceac16666273": "current",
	},
}

const (
	DefaultAcceleratorINI = `[base]
url=https://opapi.nn.com/speed/router/plug/check
channel=2
appid=nnMobile_d0k3duup
heart=https://opapi.nn.com/speed/router/heartbeat
base_url=https://opapi.nn.com/speed

[update]
domain=https://opapi.nn.com/nn-version/version/plug/upgrade

[device]
`

	DefaultFakeOSRelease = `NAME="SteamOS"
ID=steamos
ID_LIKE=arch
BUILD_ID=20260420.100
PRETTY_NAME="SteamOS"
VERSION_ID=3.6
`

	DefaultFakeProductName = "Jupiter\n"
	DefaultFakeVersion     = "20260420.1\n"
	DefaultUpgradeConfJSON = `{"reboot_conf":{"end_hour":3,"end_minute":59,"is_daily_reboot":true,"start_hour":3,"start_minute":30},"upgrade_conf":{"end_hour":23,"end_minute":59,"router_run_time":60,"start_hour":0,"start_minute":0}}` + "\n"
)

var versionRegex = regexp.MustCompile(`\b(1\.[0-9]+\.[0-9]+\.[0-9]+)\b`)

// RuntimePaths returns the canonical file layout inside installDir.
type RuntimePaths struct {
	RootDir           string
	RuntimeDir        string
	ConfigDir         string
	LogDir            string
	AccBinary         string
	UpgradeBinary     string
	XDBFile           string
	AcceleratorINI    string
	AccVersionINI     string
	UpgradeConfJSON   string
	AcceleratorMarker string
	FakeOSRelease     string
	FakeProductName   string
	FakeVersion       string
	ResolvConf        string
}

// ResolvePaths computes the directory layout under installDir.
func ResolvePaths(installDir string) RuntimePaths {
	rt := filepath.Join(installDir, "runtime")
	cfg := filepath.Join(rt, "config")
	return RuntimePaths{
		RootDir:           installDir,
		RuntimeDir:        rt,
		ConfigDir:         cfg,
		LogDir:            filepath.Join(rt, "log"),
		AccBinary:         filepath.Join(rt, "acc-gw.router.amd64"),
		UpgradeBinary:     filepath.Join(rt, "acc_upgrade_monitor"),
		XDBFile:           filepath.Join(cfg, "ipdatacloud_country.xdb"),
		AcceleratorINI:    filepath.Join(cfg, "accelerator.ini"),
		AccVersionINI:     filepath.Join(cfg, "acc_version.ini"),
		UpgradeConfJSON:   filepath.Join(cfg, "new_upgrade_conf.json"),
		AcceleratorMarker: filepath.Join(cfg, "accelerator"),
		FakeOSRelease:     filepath.Join(rt, "fake_os-release"),
		FakeProductName:   filepath.Join(rt, "fake_product_name"),
		FakeVersion:       filepath.Join(rt, "fake_version"),
		ResolvConf:        filepath.Join(rt, "resolv.conf"),
	}
}

// FileSHA256 computes the lowercase hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyAssetHash checks if a digest is in KnownValidHashes for assetName.
func VerifyAssetHash(assetName, digest string, skipVerify bool) error {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if known, ok := KnownValidHashes[assetName]; ok {
		if _, match := known[digest]; match {
			return nil
		}
	}
	if skipVerify {
		return nil
	}
	return fmt.Errorf("SHA-256 %s for %s is not in verified allowlist (pass --skip-verify if upstream updated)", digest, assetName)
}

// ExtractBinaryVersion scans acc-gw.router.amd64 for its embedded version string (e.g. "1.3.0.02").
func ExtractBinaryVersion(binPath string) string {
	data, err := os.ReadFile(binPath)
	if err != nil {
		return "1.3.0.02"
	}
	matches := versionRegex.FindAllSubmatch(data, -1)
	for _, m := range matches {
		v := string(m[1])
		if strings.HasPrefix(v, "1.") {
			return v
		}
	}
	return "1.3.0.02"
}

// EnsureAssets downloads or reuses verified upstream files and writes all runtime config files.
func EnsureAssets(installDir, stateDir, mirrorURL string, dnsServers []string, skipVerify bool, logf func(string, ...any)) error {
	paths := ResolvePaths(installDir)
	for _, dir := range []string{paths.RuntimeDir, paths.ConfigDir, paths.LogDir, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Preserve existing binding state before touching config directory
	savedBind := loadExistingBindINI(paths.AcceleratorINI, filepath.Join(stateDir, "accelerator.ini.bak"))

	if err := fetchOrReuseAsset("acc-gw.router.amd64", paths.AccBinary, mirrorURL, skipVerify, logf); err != nil {
		return err
	}
	if err := os.Chmod(paths.AccBinary, 0o755); err != nil {
		return err
	}
	// Copy acc-gw.router.amd64 to acc_upgrade_monitor so internal file checks pass
	if err := copyFileAtomic(paths.AccBinary, paths.UpgradeBinary, 0o755); err != nil {
		return fmt.Errorf("prepare acc_upgrade_monitor: %w", err)
	}

	if err := fetchOrReuseAsset("ipdatacloud_country.xdb", paths.XDBFile, mirrorURL, skipVerify, logf); err != nil {
		return err
	}

	// Detect binary version and write acc_version.ini
	ver := ExtractBinaryVersion(paths.AccBinary)
	accVerContent := fmt.Sprintf("[info]\nversion=%s\nprogram_name=acc-gw.router.amd64\n", ver)
	if err := os.WriteFile(paths.AccVersionINI, []byte(accVerContent), 0o644); err != nil {
		return err
	}

	// Write or restore accelerator.ini
	iniContent := DefaultAcceleratorINI
	if savedBind != "" {
		iniContent = savedBind
		if logf != nil {
			logf("restored existing Leigod account binding state")
		}
	}
	if err := os.WriteFile(paths.AcceleratorINI, []byte(iniContent), 0o644); err != nil {
		return err
	}
	if HasBindingToken(iniContent) {
		_ = os.WriteFile(filepath.Join(stateDir, "accelerator.ini.bak"), []byte(iniContent), 0o600)
	}

	if err := os.WriteFile(paths.UpgradeConfJSON, []byte(DefaultUpgradeConfJSON), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(paths.AcceleratorMarker, nil, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(paths.FakeOSRelease, []byte(DefaultFakeOSRelease), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(paths.FakeProductName, []byte(DefaultFakeProductName), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(paths.FakeVersion, []byte(DefaultFakeVersion), 0o644); err != nil {
		return err
	}

	return WriteResolvConf(paths.ResolvConf, dnsServers)
}

// WriteResolvConf writes a clean resolv.conf for the isolated namespace.
func WriteResolvConf(path string, dnsServers []string) error {
	if len(dnsServers) == 0 {
		dnsServers = []string{"223.5.5.5", "119.29.29.29"}
	}
	var b strings.Builder
	b.WriteString("# Generated by leigod isolated namespace manager\n")
	for _, s := range dnsServers {
		s = strings.TrimSpace(s)
		if s != "" {
			b.WriteString("nameserver ")
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// HasBindingToken checks if an accelerator.ini string contains a non-empty token.
func HasBindingToken(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "token=") && len(strings.TrimSpace(strings.TrimPrefix(line, "token="))) > 0 {
			return true
		}
	}
	return false
}

// BackupBindingState copies accelerator.ini to stateDir if it contains a valid binding token.
func BackupBindingState(installDir, stateDir string) {
	paths := ResolvePaths(installDir)
	data, err := os.ReadFile(paths.AcceleratorINI)
	if err != nil {
		return
	}
	if HasBindingToken(string(data)) {
		_ = os.MkdirAll(stateDir, 0o755)
		_ = os.WriteFile(filepath.Join(stateDir, "accelerator.ini.bak"), data, 0o600)
	}
}

func loadExistingBindINI(primaryPath, backupPath string) string {
	for _, p := range []string{primaryPath, backupPath} {
		data, err := os.ReadFile(p)
		if err == nil && HasBindingToken(string(data)) {
			return string(data)
		}
	}
	return ""
}

func fetchOrReuseAsset(assetName, destPath, mirrorURL string, skipVerify bool, logf func(string, ...any)) error {
	// 1. Check if destPath or /tmp/<assetName> already exists and passes hash verification
	candidates := []string{
		destPath,
		filepath.Join("/tmp", assetName),
		filepath.Join("/opt/leigod", assetName),
		filepath.Join("/opt/leigod/config", assetName),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		digest, err := FileSHA256(candidate)
		if err == nil && VerifyAssetHash(assetName, digest, false) == nil {
			if candidate != destPath {
				if logf != nil {
					logf("reusing verified local %s (%s)", assetName, digest[:12])
				}
				if err := copyFileAtomic(candidate, destPath, 0o644); err != nil {
					return err
				}
			} else if logf != nil {
				logf("verified existing %s (%s)", assetName, digest[:12])
			}
			return nil
		}
	}

	// 2. Download from mirrorURL
	url := strings.TrimRight(mirrorURL, "/") + "/" + assetName
	if logf != nil {
		logf("downloading %s from %s ...", assetName, url)
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), "."+assetName+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", assetName, err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), resp.Body); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("read %s body: %w", assetName, err)
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if err := VerifyAssetHash(assetName, digest, skipVerify); err != nil {
		return err
	}
	if logf != nil {
		logf("verified %s SHA-256: %s", assetName, digest)
	}
	return os.Rename(tmpPath, destPath)
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".copy.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, dst)
}

// DeriveOrLoadDeviceMAC returns a persistent locally-administered unicast MAC address.
// 设备 MAC 不是敏感信息（会展示在面板上），因此落盘权限为 0644。
func DeriveOrLoadDeviceMAC(configuredMAC, stateDir, machineIDPath string) (string, error) {
	want := strings.ToLower(strings.TrimSpace(configuredMAC))
	if want != "" && want != "auto" {
		if _, err := net.ParseMAC(want); err != nil {
			return "", fmt.Errorf("invalid device_mac %q: %w", configuredMAC, err)
		}
		persistMAC(stateDir, want)
		return want, nil
	}

	macFile := filepath.Join(stateDir, "device-mac")
	if data, err := os.ReadFile(macFile); err == nil {
		saved := strings.ToLower(strings.TrimSpace(string(data)))
		if _, err := net.ParseMAC(saved); err == nil {
			persistMAC(stateDir, saved)
			return saved, nil
		}
	}

	var seed []byte
	if machineIDPath != "" {
		if data, err := os.ReadFile(machineIDPath); err == nil {
			seed = bytes.TrimSpace(data)
		}
	}
	if len(seed) == 0 {
		seed = []byte("leigod-linux-default-seed")
	}

	sum := md5.Sum(seed)
	mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
	persistMAC(stateDir, mac)
	return mac, nil
}

// persistMAC 保证 device-mac 存在且对所有用户可读（面板需要展示）。
func persistMAC(stateDir, mac string) {
	if stateDir == "" {
		return
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	macFile := filepath.Join(stateDir, "device-mac")
	_ = os.WriteFile(macFile, []byte(mac+"\n"), 0o644)
	_ = os.Chmod(macFile, 0o644)
}
