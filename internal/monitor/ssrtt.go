package monitor

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ssUsersRe   = regexp.MustCompile(`users:\(\("([^"]+)",pid=([0-9]+)`)
	ssRTTRe     = regexp.MustCompile(`\brtt:([0-9.]+)/([0-9.]+)`)
	ssMinRTTRe  = regexp.MustCompile(`\bminrtt:([0-9.]+)`)
	ssRetransRe = regexp.MustCompile(`\bretrans:([0-9]+)(?:/([0-9]+))?`)
	ssUnackedRe = regexp.MustCompile(`\bunacked:([0-9]+)`)
	ssStateRe   = regexp.MustCompile(`^(ESTAB|SYN-SENT|SYN-RECV|FIN-WAIT-1|FIN-WAIT-2|TIME-WAIT|CLOSE-WAIT|LAST-ACK|CLOSING|LISTEN|UNCONN)\s`)
)

// detectGameRTT 通过 `leigod run -- ss -tin` 在命名空间内读取 TCP 连接的真实 RTT。
// 使用 leigod run 而不是 ip netns exec，因此普通用户（已安装 cap_sys_admin 跳板）也能跑。
func (m *Monitor) detectGameRTT(g GameInfo) GameRTT {
	out := GameRTT{}

	self, err := os.Executable()
	if err != nil {
		out.Note = "无法定位可执行文件"
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "run", "--", "ss", "-tin")
	cmd.Env = os.Environ()
	raw, err := cmd.Output()
	if err != nil && len(raw) == 0 {
		if ctx.Err() != nil {
			out.Note = "读取 ss 超时"
		} else {
			out.Note = "无法在命名空间内执行 ss: " + err.Error()
		}
		return out
	}

	conns := parseSSInfo(string(raw))

	instIP := ""
	if g.Instance != "" {
		instIP = strings.Split(g.Instance, ":")[0]
	}

	for i := range conns {
		c := &conns[i]
		peerIP, peerPort := splitHostPort(c.Peer)
		switch {
		case peerPort == "6112" || (instIP != "" && peerIP == instIP):
			c.Kind = "game_server"
		case peerPort == "5438" || peerPort == "5443" || peerPort == "5452":
			c.Kind = "leigod_node"
		default:
			c.Kind = "other"
		}
		c.Game = c.Kind == "game_server"
		if v, ok := m.accStateLocked(c.Peer); ok {
			c.Acc = v
		}
		// middleware 侧连到节点 → 说明这条游戏连接走了加速
		if c.Role == "middleware" && c.Kind == "leigod_node" {
			c.Acc = true
		}
	}

	out.Conns = conns
	out.Available = true

	// 真实链路 RTT 取 middleware 侧（acc-gw 自己发出）的连接：
	//   * 连到雷神节点 5438 → 加速链路
	//   * 连到游戏服务器 6112 → 直连兜底链路
	var sum float64
	for _, c := range conns {
		if c.Role != "middleware" {
			continue
		}
		if c.Kind != "game_server" && c.Kind != "leigod_node" {
			continue
		}
		out.Retrans += c.Retrans
		if c.RTTms > out.WorstMs {
			out.WorstMs = c.RTTms
		}
		if out.BestMs == 0 || c.RTTms < out.BestMs {
			out.BestMs = c.RTTms
		}
		sum += c.RTTms
		if c.Kind == "leigod_node" {
			out.AccCount++
		} else {
			out.DirCount++
		}
	}
	if n := out.AccCount + out.DirCount; n > 0 {
		out.AvgMs = sum / float64(n)
	} else {
		out.Note = "未发现可测量的游戏链路"
	}
	return out
}

func (m *Monitor) accStateLocked(peer string) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accState == nil {
		return false, false
	}
	v, ok := m.accState[peer]
	return v, ok
}

// parseSSInfo 解析 `ss -tin` 输出，返回带 RTT/重传信息的 TCP 连接。
func parseSSInfo(text string) []ConnRTT {
	var conns []ConnRTT
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !ssStateRe.MatchString(line) {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		state := f[0]
		if state == "LISTEN" || state == "UNCONN" {
			continue
		}
		recvQ, _ := strconv.Atoi(f[1])
		sendQ, _ := strconv.Atoi(f[2])
		c := ConnRTT{
			Local: f[3],
			Peer:  f[4],
			RecvQ: recvQ,
			SendQ: sendQ,
			Role:  "middleware",
		}
		if mm := ssUsersRe.FindStringSubmatch(line); len(mm) == 3 {
			c.Role = "game"
			c.Proc = mm[1]
		}
		// 下一行是 socket 明细
		if i+1 < len(lines) {
			detail := lines[i+1]
			if mm := ssRTTRe.FindStringSubmatch(detail); len(mm) == 3 {
				c.RTTms, _ = strconv.ParseFloat(mm[1], 64)
			}
			if mm := ssMinRTTRe.FindStringSubmatch(detail); len(mm) == 2 {
				c.MinRTTms, _ = strconv.ParseFloat(mm[1], 64)
			}
			if mm := ssRetransRe.FindStringSubmatch(detail); len(mm) >= 2 {
				c.Retrans, _ = strconv.Atoi(mm[1])
			}
			if mm := ssUnackedRe.FindStringSubmatch(detail); len(mm) == 2 {
				c.Unacked, _ = strconv.Atoi(mm[1])
			}
			if c.RTTms > 0 {
				conns = append(conns, c)
			}
		}
	}
	return conns
}

func splitHostPort(s string) (string, string) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}
