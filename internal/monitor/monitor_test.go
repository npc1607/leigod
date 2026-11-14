package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleLog = `2026-11-14 11:14:29.589 INFO  [TunnelHeartBeatIo.cpp] [heart_beat:315] [0x1]  send heart to server(fd:9):150.242.96.186:13448 connect_id:1109857536 success, record size:1
2026-11-14 11:14:29.600 INFO  [TunnelHeartBeatIo.cpp] [on_read:85] [0x1]  recv heart response from server(fd:9):150.242.96.186:13448 success, current delay:23 ms
2026-11-14 11:14:29.601 INFO  [TunnelHeartBeatIo.cpp] [check_heart_statistics:196] [0x1]  server(fd:9):150.242.96.186:13448 connect_id:1109857536 lost_persent:0 average_delay:23 ms record_count:7 compete_count:7
2026-11-14 11:14:29.546 INFO  [GameAccMgr.cpp] [domain_acc_check:228]  domain_acc_check domain:web.poecdn.com in whitelist use line:1, go acc
2026-11-14 11:14:30.100 INFO  [GameAccMgr.cpp] [domain_acc_check:305]  domain_acc_check domain:cp.cloudflare.com not in whitelist, and current acc mode is no package mode, go direct
2026-11-14 11:14:30.200 INFO  [TcpIoUnit.cpp] [connect_server:627]  fd(11) src 10.254.254.2:55260 to dest 104.17.255.156:443 by local 10.254.254.2:55264 to s5 server 150.242.96.186:13448, is_acc(1), is_dns(0), domain(www.pathofexile.com), is_https_hijack:0
`

func TestTailerParsesLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acc_PC.log")
	if err := os.WriteFile(p, []byte(sampleLog), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(Config{LogPath: p})
	m.Poll()

	st := m.Stats()
	if st.DelayMs != 23 {
		t.Errorf("DelayMs = %d, want 23", st.DelayMs)
	}
	if st.Records != 7 {
		t.Errorf("Records = %d, want 7", st.Records)
	}
	if st.Node != "150.242.96.186:13448" {
		t.Errorf("Node = %q", st.Node)
	}
	if st.AccHits != 1 || st.DirectHits != 1 {
		t.Errorf("hits acc=%d direct=%d, want 1/1", st.AccHits, st.DirectHits)
	}
	if st.Samples != 1 {
		t.Errorf("Samples = %d, want 1", st.Samples)
	}
	if st.Recent != 1 {
		t.Errorf("Recent = %d, want 1", st.Recent)
	}
}

func TestTailerIncrementalRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acc_PC.log")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(Config{LogPath: p})
	m.Poll()
	if got := m.Stats().AccHits; got != 0 {
		t.Fatalf("initial AccHits = %d", got)
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("2026-11-14 11:14:29.546 INFO [GameAccMgr.cpp] domain_acc_check domain:a.poecdn.com in whitelist use line:1, go acc\n")
	_ = f.Close()

	m.Poll()
	if got := m.Stats().AccHits; got != 1 {
		t.Fatalf("after append AccHits = %d, want 1", got)
	}

	// 再 Poll 一次不应重复计数
	m.Poll()
	if got := m.Stats().AccHits; got != 1 {
		t.Fatalf("idempotent AccHits = %d, want 1", got)
	}

	// 截断后重新读取
	if err := os.WriteFile(p, []byte("2026-11-14 11:14:29.546 INFO [GameAccMgr.cpp] domain_acc_check domain:b.poecdn.com in whitelist use line:1, go acc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Poll()
	if got := m.Stats().AccHits; got != 2 {
		t.Fatalf("after truncate AccHits = %d, want 2", got)
	}
}

func TestParseClientLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Client.txt")
	content := `2026/11/14 10:28:43 ***** LOG FILE OPENING *****
2026/11/14 10:28:43 5918395 [INFO Client 340] [JOB] Start
2026/11/14 10:44:03 8350136 [WARN Client 340] Unable to init Steam API
2026/11/14 11:14:27 ***** LOG FILE OPENING *****
2026/11/14 11:14:27 8662496 [INFO Client 340] [HTTP2] User agent: PoE release/tags/3.30.0
2026/11/14 11:15:02 8662496 [INFO Client 340] Connecting to instance server at 103.194.165.61:6112
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ok, inst := parseClientLog(p)
	if !ok {
		t.Error("expected SteamAPI OK for last session (old session had the failure)")
	}
	if inst != "103.194.165.61:6112" {
		t.Errorf("Instance = %q", inst)
	}
}
