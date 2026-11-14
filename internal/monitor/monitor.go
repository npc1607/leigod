// Package monitor 采集雷神加速器的实时运行状态：隧道心跳、延迟曲线、
// 加速/直连命中统计、最近连接，以及服务/命名空间/游戏进程等系统状态。
package monitor

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxHistory = 240 // 延迟曲线保留点数
	maxRecent  = 80  // 最近连接保留条数
	maxScan    = 8 << 20
)

var (
	tsRe     = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)`)
	hbRe     = regexp.MustCompile(`current delay:(\d+) ms`)
	statRe   = regexp.MustCompile(`lost_persent:(\d+) average_delay:(\d+) ms record_count:(\d+)`)
	serverRe = regexp.MustCompile(`send heart to server\(fd:\d+\):([0-9.]+):(\d+)`)
	hitRe    = regexp.MustCompile(`domain_acc_check domain:(\S+) .*(go acc|go direct)`)
	connRe   = regexp.MustCompile(`src (\S+) to dest (\S+) by local \S+ to s5 server (\S+), is_acc\((\d)\).*domain\(([^)]*)\)`)
)

// Sample 是延迟曲线上的一个采样点。
type Sample struct {
	T     int64 `json:"t"`
	Delay int   `json:"delay"`
	Loss  int   `json:"loss"`
}

// Conn 是一条最近的连接记录。
type Conn struct {
	Time   string `json:"time"`
	Domain string `json:"domain"`
	Dest   string `json:"dest"`
	Node   string `json:"node"`
	Acc    bool   `json:"acc"`
}

// ConnRTT 是命名空间内一条 TCP 连接的真实 RTT（来自 ss -tin）。
type ConnRTT struct {
	Peer     string  `json:"peer"`
	Local    string  `json:"local"`
	RTTms    float64 `json:"rtt_ms"`
	MinRTTms float64 `json:"min_rtt_ms"`
	Retrans  int     `json:"retrans"`
	Unacked  int     `json:"unacked"`
	SendQ    int     `json:"send_q"`
	RecvQ    int     `json:"recv_q"`
	Game     bool    `json:"game"`
	Acc      bool    `json:"acc"`
	Role     string  `json:"role"` // game（游戏进程自己）/ middleware（acc-gw）
	Kind     string  `json:"kind"` // game_server / leigod_node / other
	Proc     string  `json:"proc"`
}

// GameRTT 是游戏连接延迟汇总。
type GameRTT struct {
	Conns     []ConnRTT `json:"conns"`
	WorstMs   float64   `json:"worst_ms"`
	BestMs    float64   `json:"best_ms"`
	AvgMs     float64   `json:"avg_ms"`
	AccCount  int       `json:"acc_count"`
	DirCount  int       `json:"dir_count"`
	Retrans   int       `json:"retrans"`
	Available bool      `json:"available"`
	Note      string    `json:"note"`
}

// Config 是采集器的配置。
type Config struct {
	LogPath     string
	Namespace   string
	ServiceName string
	RouteTable  int
	InstallDir  string
	StateDir    string
	WebPort     int
	WebListen   string
}

// Monitor 持有增量日志解析状态。
type Monitor struct {
	Cfg Config

	mu       sync.Mutex
	offset   int64
	partial  []byte
	lastMod  time.Time
	delay    int
	loss     int
	records  int
	node     string
	lastBeat time.Time
	accHits  int64
	dirHits  int64
	history  []Sample
	recent   []Conn
	accState map[string]bool // "ip:port" -> 是否走加速（来自日志 is_acc）
	lastErr  string
}

// New 创建一个采集器。
func New(cfg Config) *Monitor {
	return &Monitor{Cfg: cfg, accState: map[string]bool{}}
}

// Poll 增量读取日志并更新统计。
func (m *Monitor) Poll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollLocked()
}

func (m *Monitor) pollLocked() {
	f, err := os.Open(m.Cfg.LogPath)
	if err != nil {
		m.lastErr = err.Error()
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	mod := info.ModTime()
	if size < m.offset {
		// 日志被截断或轮转
		m.offset = 0
		m.partial = nil
	}
	if size == m.offset {
		// 大小没变但 mtime 变了 → 文件被重写（长度相同），从头重读
		if m.offset > 0 && !mod.Equal(m.lastMod) {
			m.offset = 0
			m.partial = nil
		} else {
			return
		}
	}
	m.lastMod = mod
	start := m.offset
	if size-start > maxScan {
		start = size - maxScan
		m.partial = nil
	}
	if _, err := f.Seek(start, 0); err != nil {
		return
	}

	r := bufio.NewReaderSize(f, 64*1024)
	var consumed int64
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// 末尾不完整行留到下次
			m.partial = append(m.partial, line...)
			break
		}
		consumed += int64(len(line))
		full := line
		if len(m.partial) > 0 {
			full = append(m.partial, line...)
			m.partial = nil
		}
		m.parseLine(bytes.TrimRight(full, "\r\n"))
	}
	m.offset = start + consumed
	m.lastErr = ""
}

func (m *Monitor) parseLine(line []byte) {
	if len(line) == 0 {
		return
	}
	s := string(line)

	var ts string
	if mm := tsRe.FindStringSubmatch(s); len(mm) == 2 {
		ts = mm[1]
	}

	if mm := serverRe.FindStringSubmatch(s); len(mm) == 3 {
		m.node = mm[1] + ":" + mm[2]
	}

	if mm := hbRe.FindStringSubmatch(s); len(mm) == 2 {
		if v, err := strconv.Atoi(mm[1]); err == nil {
			m.delay = v
			m.lastBeat = time.Now()
			m.history = append(m.history, Sample{T: time.Now().Unix(), Delay: v, Loss: m.loss})
			if len(m.history) > maxHistory {
				m.history = m.history[len(m.history)-maxHistory:]
			}
		}
	}

	if mm := statRe.FindStringSubmatch(s); len(mm) == 4 {
		loss, e1 := strconv.Atoi(mm[1])
		delay, e2 := strconv.Atoi(mm[2])
		recs, e3 := strconv.Atoi(mm[3])
		if e1 == nil {
			m.loss = loss
		}
		if e2 == nil {
			m.delay = delay
		}
		if e3 == nil {
			m.records = recs
		}
	}

	if mm := hitRe.FindStringSubmatch(s); len(mm) == 3 {
		if mm[2] == "go acc" {
			m.accHits++
		} else {
			m.dirHits++
		}
	}

	if mm := connRe.FindStringSubmatch(s); len(mm) == 6 {
		isAcc := mm[4] == "1"
		m.accState[mm[2]] = isAcc
		if isAcc || len(m.recent) < maxRecent {
			m.recent = append([]Conn{{
				Time:   ts,
				Dest:   mm[2],
				Node:   mm[3],
				Acc:    isAcc,
				Domain: mm[5],
			}}, m.recent...)
			if len(m.recent) > maxRecent {
				m.recent = m.recent[:maxRecent]
			}
		}
	}
}

// Stats 返回当前累计的解析统计（供测试与 CLI 使用）。
type Stats struct {
	DelayMs    int
	LossPct    int
	Records    int
	Node       string
	AccHits    int64
	DirectHits int64
	Samples    int
	Recent     int
}

// Stats 返回解析统计快照。
func (m *Monitor) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		DelayMs:    m.delay,
		LossPct:    m.loss,
		Records:    m.records,
		Node:       m.node,
		AccHits:    m.accHits,
		DirectHits: m.dirHits,
		Samples:    len(m.history),
		Recent:     len(m.recent),
	}
}

// Snapshot 是完整的状态快照。
type Snapshot struct {
	Now        string      `json:"now"`
	Service    ServiceInfo `json:"service"`
	Namespace  NSInfo      `json:"namespace"`
	Device     DeviceInfo  `json:"device"`
	WAN        WANInfo     `json:"wan"`
	Tunnel     TunnelInfo  `json:"tunnel"`
	Traffic    TrafficInfo `json:"traffic"`
	Game       GameInfo    `json:"game"`
	GameRTT    GameRTT     `json:"game_rtt"`
	Ports      PortsInfo   `json:"ports"`
	History    []Sample    `json:"history"`
	Recent     []Conn      `json:"recent"`
	LogPath    string      `json:"log_path"`
	LogError   string      `json:"log_error"`
	PolledUnix int64       `json:"polled_unix"`
}

type ServiceInfo struct {
	Active  bool   `json:"active"`
	Enabled bool   `json:"enabled"`
	Name    string `json:"name"`
}

type NSInfo struct {
	Exists     bool   `json:"exists"`
	Name       string `json:"name"`
	TUN        string `json:"tun"`
	AccRunning bool   `json:"acc_running"`
	DaemonRun  bool   `json:"daemon_running"`
	SocksRun   bool   `json:"socks_running"`
}

type DeviceInfo struct {
	MAC   string `json:"mac"`
	Bound bool   `json:"bound"`
	LANIP string `json:"lan_ip"`
}

type WANInfo struct {
	Iface   string `json:"iface"`
	Gateway string `json:"gateway"`
	LocalIP string `json:"local_ip"`
	Table   int    `json:"table"`
	Bypass  bool   `json:"bypass"`
	Raw     string `json:"raw"`
}

type TunnelInfo struct {
	Active    bool   `json:"active"`
	Node      string `json:"node"`
	DelayMs   int    `json:"delay_ms"`
	LossPct   int    `json:"loss_pct"`
	Records   int    `json:"records"`
	LastBeat  string `json:"last_beat"`
	BeatFresh bool   `json:"beat_fresh"`
}

type TrafficInfo struct {
	AccHits    int64 `json:"acc_hits"`
	DirectHits int64 `json:"direct_hits"`
}

type GameInfo struct {
	PIDs       []int  `json:"pids"`
	InNS       int    `json:"in_ns"`
	Total      int    `json:"total"`
	SteamAPIOK bool   `json:"steamapi_ok"`
	Instance   string `json:"instance"`
	ClientLog  string `json:"client_log"`
}

type PortsInfo struct {
	Web         bool `json:"web"`
	Socks       bool `json:"socks"`
	SteamHost   bool `json:"steam_host"`
	SteamBridge bool `json:"steam_bridge"`
	SteamNS     bool `json:"steam_ns"`
}

// Snapshot 采集一次完整状态（含系统命令探测，耗时约几十毫秒）。
func (m *Monitor) Snapshot() Snapshot {
	m.Poll()

	m.mu.Lock()
	delay, loss, records := m.delay, m.loss, m.records
	node := m.node
	lastBeat := m.lastBeat
	accHits, dirHits := m.accHits, m.dirHits
	history := append([]Sample(nil), m.history...)
	recent := append([]Conn(nil), m.recent...)
	logErr := m.lastErr
	m.mu.Unlock()

	snap := Snapshot{
		Now:      time.Now().Format("2006-01-02 15:04:05"),
		History:  history,
		Recent:   recent,
		LogPath:  m.Cfg.LogPath,
		LogError: logErr,
	}

	snap.Service = ServiceInfo{
		Name:    m.Cfg.ServiceName,
		Active:  runOK("systemctl", "is-active", "--quiet", m.Cfg.ServiceName),
		Enabled: runOK("systemctl", "is-enabled", "--quiet", m.Cfg.ServiceName),
	}

	nsPath := "/run/netns/" + m.Cfg.Namespace
	if _, err := os.Stat(nsPath); err == nil {
		snap.Namespace.Exists = true
	}
	snap.Namespace.Name = m.Cfg.Namespace
	snap.Namespace.AccRunning = procMatch("acc-gw.router", "-r acc")
	snap.Namespace.DaemonRun = procMatch("acc-gw.router", "-r daemon")
	snap.Namespace.SocksRun = procMatch("leigod", "_ns-socks5")
	snap.Namespace.TUN = m.detectTUN()

	// 设备身份与绑定
	if data, err := os.ReadFile(m.Cfg.StateDir + "/device-mac"); err == nil {
		snap.Device.MAC = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile(m.Cfg.InstallDir + "/runtime/config/accelerator.ini"); err == nil {
		snap.Device.Bound = hasToken(string(data))
	}

	// 策略路由
	wan, raw := m.detectWAN()
	snap.WAN = wan
	snap.WAN.Raw = raw
	if wan.LocalIP != "" {
		snap.Device.LANIP = wan.LocalIP
	}

	snap.Tunnel = TunnelInfo{
		// 以加速进程为准（普通用户看不到 ns 内网卡名，非 root 时 TUN 可能为空）
		Active:    snap.Namespace.AccRunning,
		Node:      node,
		DelayMs:   delay,
		LossPct:   loss,
		Records:   records,
		BeatFresh: time.Since(lastBeat) < 20*time.Second,
	}
	if !lastBeat.IsZero() {
		snap.Tunnel.LastBeat = lastBeat.Format("15:04:05")
	}
	snap.Traffic = TrafficInfo{AccHits: accHits, DirectHits: dirHits}

	snap.Game = m.detectGame()
	snap.GameRTT = m.detectGameRTT(snap.Game)

	listen := listenPorts()
	snap.Ports = PortsInfo{
		Web:         listen[m.Cfg.WebListen],
		Socks:       listen["127.0.0.1:15588"],
		SteamHost:   listen["127.0.0.1:57343"],
		SteamBridge: listen["10.254.254.1:57343"],
		SteamNS:     m.nsListening(),
	}
	snap.PolledUnix = time.Now().Unix()
	return snap
}

func (m *Monitor) detectTUN() string {
	out, err := exec.Command("ip", "-n", m.Cfg.Namespace, "-o", "link", "show").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		if strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "acc") {
			return name
		}
	}
	return ""
}

func (m *Monitor) detectWAN() (WANInfo, string) {
	info := WANInfo{Table: m.Cfg.RouteTable}
	out, err := exec.Command("ip", "-4", "route", "show", "table", strconv.Itoa(m.Cfg.RouteTable)).Output()
	if err != nil {
		return info, ""
	}
	raw := strings.TrimSpace(string(out))
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "default" {
			continue
		}
		for i := 0; i < len(f)-1; i++ {
			switch f[i] {
			case "via":
				info.Gateway = f[i+1]
			case "dev":
				info.Iface = f[i+1]
			case "src":
				info.LocalIP = f[i+1]
			}
		}
		break
	}
	if info.Iface != "" {
		bad := []string{"tun", "tap", "nekobox", "clash", "sing", "dae", "wg", "throne", "meta"}
		info.Bypass = true
		for _, p := range bad {
			if strings.HasPrefix(strings.ToLower(info.Iface), p) {
				info.Bypass = false
				break
			}
		}
	}
	return info, raw
}

func (m *Monitor) detectGame() GameInfo {
	gi := GameInfo{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return gi
	}
	hostNS, _ := os.Readlink("/proc/self/ns/net")
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(cmdline) == 0 {
			continue
		}
		s := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(s, "PathOfExileSteam.exe") {
			continue
		}
		// 排除 Steam 的包装层（launcher shell / supervisor），它们只是命令行里带了游戏名
		if strings.Contains(s, "steam-launch-wrapper") ||
			strings.Contains(s, "steam-runtime-supervisor") ||
			strings.Contains(s, "--alongside-steam") {
			continue
		}
		gi.Total++
		gi.PIDs = append(gi.PIDs, pid)
		if ns, err := os.Readlink("/proc/" + e.Name() + "/ns/net"); err == nil && hostNS != "" && ns != hostNS {
			gi.InNS++
		}
	}

	// PoE 客户端日志
	for _, p := range []string{
		"/mnt/ntfs-game/SteamLibrary/steamapps/common/Path of Exile/logs/Client.txt",
	} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		gi.ClientLog = p
		gi.SteamAPIOK, gi.Instance = parseClientLog(p)
		break
	}
	return gi
}

func parseClientLog(path string) (steamOK bool, instance string) {
	f, err := os.Open(path)
	if err != nil {
		return true, ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return true, ""
	}
	// 只读最后 256KB
	start := info.Size() - 256<<10
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, 0); err != nil {
		return true, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true, ""
	}
	if start > 0 {
		if i := bytes.IndexByte(data[start:], '\n'); i >= 0 {
			data = data[start+int64(i)+1:]
		} else {
			data = nil
		}
	} else {
		data = data[start:]
	}

	lines := strings.Split(string(data), "\n")
	// 找最后一次 LOG FILE OPENING，只看那之后的记录
	sessionStart := 0
	for i, l := range lines {
		if strings.Contains(l, "LOG FILE OPENING") {
			sessionStart = i
		}
	}
	session := lines[sessionStart:]
	for _, l := range session {
		if strings.Contains(l, "Unable to init Steam API") {
			steamOK = false
		}
		if strings.Contains(l, "Connecting to instance server at ") {
			if i := strings.Index(l, "Connecting to instance server at "); i >= 0 {
				rest := l[i+len("Connecting to instance server at "):]
				instance = strings.TrimSpace(rest)
			}
		}
	}
	if !strings.Contains(strings.Join(session, "\n"), "Unable to init Steam API") {
		steamOK = true
	}
	return steamOK, instance
}

func (m *Monitor) nsListening() bool {
	// 127.0.0.1:57343 在 ns 内监听，需要 root 才能看到别的 ns 的 socket；
	// 退化为检查 ns 内 SS 输出。
	out, err := exec.Command("ip", "netns", "exec", m.Cfg.Namespace, "ss", "-ltn").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "127.0.0.1:57343")
}

func listenPorts() map[string]bool {
	res := map[string]bool{}
	out, err := exec.Command("ss", "-ltn").Output()
	if err != nil {
		return res
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		res[f[3]] = true
	}
	return res
}

func runOK(name string, args ...string) bool {
	c := exec.Command(name, args...)
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		return false
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(3 * time.Second):
		_ = c.Process.Kill()
		return false
	}
}

func procMatch(substr, argsSubstr string) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		s := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if strings.Contains(s, substr) && strings.Contains(s, argsSubstr) {
			return true
		}
	}
	return false
}

func hasToken(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "token=") && len(strings.TrimSpace(strings.TrimPrefix(line, "token="))) > 0 {
			return true
		}
	}
	return false
}
