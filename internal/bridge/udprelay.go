package bridge

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// UDPRelay listens on ListenAddr (e.g. 0.0.0.0:6066) for LAN broadcast/unicast
// discovery packets from the Leigod mobile app (<NBOX> WHO / <NBOX> HELLO)
// and relays them to TargetAddr (e.g. 10.254.254.2:6066) inside the namespace.
type UDPRelay struct {
	ListenAddr   string
	TargetAddr   string
	ReplyTimeout time.Duration
	Logf         func(string, ...any)

	mu   sync.Mutex
	conn net.PacketConn
}

// Start runs the UDP relay until ctx is cancelled.
func (r *UDPRelay) Start(ctx context.Context) error {
	if r.ReplyTimeout <= 0 {
		r.ReplyTimeout = 2 * time.Second
	}
	pc, err := net.ListenPacket("udp4", r.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen udp relay %s: %w", r.ListenAddr, err)
	}
	r.mu.Lock()
	r.conn = pc
	r.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	targetUDP, err := net.ResolveUDPAddr("udp4", r.TargetAddr)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("resolve udp target %s: %w", r.TargetAddr, err)
	}

	buf := make([]byte, 4096)
	for {
		n, clientAddr, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		payload := append([]byte(nil), buf[:n]...)
		go r.forwardOne(pc, clientAddr, targetUDP, payload)
	}
}

// Addr returns the bound local UDP address.
func (r *UDPRelay) Addr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return nil
	}
	return r.conn.LocalAddr()
}

func (r *UDPRelay) forwardOne(listener net.PacketConn, clientAddr net.Addr, targetAddr *net.UDPAddr, payload []byte) {
	upConn, err := net.DialUDP("udp4", nil, targetAddr)
	if err != nil {
		if r.Logf != nil {
			r.Logf("udprelay: dial %s failed: %v", targetAddr, err)
		}
		return
	}
	defer upConn.Close()

	_ = upConn.SetDeadline(time.Now().Add(r.ReplyTimeout))
	if _, err := upConn.Write(payload); err != nil {
		return
	}

	resp := make([]byte, 4096)
	n, err := upConn.Read(resp)
	if err != nil || n == 0 {
		return
	}
	_, _ = listener.WriteTo(resp[:n], clientAddr)
}
