package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	socks5Version = 0x05
	authNone      = 0x00

	cmdConnect      = 0x01
	cmdUDPAssociate = 0x03

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess              = 0x00
	repGeneralFailure       = 0x01
	repCommandNotSupported  = 0x07
	repAddressNotSupported  = 0x08
)

// Server implements a lightweight RFC 1928 SOCKS5 server supporting both
// TCP CONNECT and UDP ASSOCIATE so game TCP + UDP traffic can be routed
// into the isolated Leigod network namespace.
type Server struct {
	ListenAddr  string
	AnnounceIP  net.IP // Optional override IP for UDP ASSOCIATE BND.ADDR
	DialTimeout time.Duration
	Logf        func(string, ...any)

	mu       sync.Mutex
	listener net.Listener
}

// Start runs the SOCKS5 server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	if s.DialTimeout <= 0 {
		s.DialTimeout = 10 * time.Second
	}
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("socks5 listen %s: %w", s.ListenAddr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handleConn(ctx, conn)
	}
}

// Addr returns the bound listener address.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// 1. Greeting: VER, NMETHODS, METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if hdr[0] != socks5Version {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	// Select No Auth (0x00)
	if _, err := conn.Write([]byte{socks5Version, authNone}); err != nil {
		return
	}

	// 2. Request: VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT
	reqHdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, reqHdr); err != nil {
		return
	}
	if reqHdr[0] != socks5Version {
		return
	}
	cmd := reqHdr[1]
	atyp := reqHdr[3]

	targetAddr, err := readAddrSpec(conn, atyp)
	if err != nil {
		_ = writeReply(conn, repAddressNotSupported, nil)
		return
	}

	switch cmd {
	case cmdConnect:
		s.handleConnect(ctx, conn, targetAddr)
	case cmdUDPAssociate:
		s.handleUDPAssociate(ctx, conn)
	default:
		_ = writeReply(conn, repCommandNotSupported, nil)
	}
}

func (s *Server) handleConnect(ctx context.Context, clientConn net.Conn, targetAddr string) {
	dialer := net.Dialer{Timeout: s.DialTimeout}
	upConn, err := dialer.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		_ = writeReply(clientConn, repGeneralFailure, nil)
		return
	}
	defer upConn.Close()

	localTCP, _ := upConn.LocalAddr().(*net.TCPAddr)
	if err := writeReply(clientConn, repSuccess, localTCP); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upConn, clientConn)
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, upConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	wg.Wait()
}

func (s *Server) handleUDPAssociate(ctx context.Context, tcpConn net.Conn) {
	localTCPAddr, ok := tcpConn.LocalAddr().(*net.TCPAddr)
	if !ok {
		_ = writeReply(tcpConn, repGeneralFailure, nil)
		return
	}
	bindIP := localTCPAddr.IP
	if len(s.AnnounceIP) > 0 {
		bindIP = s.AnnounceIP
	}

	// Listen for client UDP datagrams on the namespace interface
	clientRelay, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		_ = writeReply(tcpConn, repGeneralFailure, nil)
		return
	}
	defer clientRelay.Close()

	// Outbound socket towards remote game servers inside the namespace
	remoteSocket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		_ = writeReply(tcpConn, repGeneralFailure, nil)
		return
	}
	defer remoteSocket.Close()

	relayUDPAddr, _ := clientRelay.LocalAddr().(*net.UDPAddr)
	replyTCPAddr := &net.TCPAddr{IP: bindIP, Port: relayUDPAddr.Port}
	if err := writeReply(tcpConn, repSuccess, replyTCPAddr); err != nil {
		return
	}

	var clientUDPAddrMu sync.RWMutex
	var clientUDPAddr *net.UDPAddr

	done := make(chan struct{})
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			close(done)
			_ = clientRelay.Close()
			_ = remoteSocket.Close()
		})
	}
	defer closeAll()

	go func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	}()

	// Pump: Client -> Remote Game Server
	go func() {
		buf := make([]byte, 65535)
		for {
			n, srcAddr, err := clientRelay.ReadFromUDP(buf)
			if err != nil {
				return
			}
			// Parse SOCKS5 UDP header: RSV(2), FRAG(1), ATYP(1), DST.ADDR, DST.PORT, DATA
			dstAddr, payload, err := parseUDPDatagram(buf[:n])
			if err != nil || dstAddr == nil {
				continue
			}
			clientUDPAddrMu.Lock()
			clientUDPAddr = srcAddr
			clientUDPAddrMu.Unlock()

			_, _ = remoteSocket.WriteToUDP(payload, dstAddr)
		}
	}()

	// Pump: Remote Game Server -> Client
	go func() {
		buf := make([]byte, 65535)
		for {
			n, raddr, err := remoteSocket.ReadFromUDP(buf)
			if err != nil {
				return
			}
			clientUDPAddrMu.RLock()
			cAddr := clientUDPAddr
			clientUDPAddrMu.RUnlock()
			if cAddr == nil {
				continue
			}
			pkt := buildUDPDatagram(raddr, buf[:n])
			if len(pkt) > 0 {
				_, _ = clientRelay.WriteToUDP(pkt, cAddr)
			}
		}
	}()

	// Wait until the control TCP connection closes (per RFC 1928)
	discard := make([]byte, 128)
	for {
		if _, err := tcpConn.Read(discard); err != nil {
			return
		}
	}
}

func readAddrSpec(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case atypIPv4:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(r, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case atypIPv6:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(r, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case atypDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return "", err
		}
		domain := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(r, domain); err != nil {
			return "", err
		}
		host = string(domain)
	default:
		return "", fmt.Errorf("unsupported atyp %d", atyp)
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf)
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

func writeReply(w io.Writer, rep byte, bindAddr *net.TCPAddr) error {
	ip4 := net.IPv4zero.To4()
	var port uint16
	if bindAddr != nil {
		if v4 := bindAddr.IP.To4(); v4 != nil {
			ip4 = v4
		}
		port = uint16(bindAddr.Port)
	}
	buf := make([]byte, 10)
	buf[0] = socks5Version
	buf[1] = rep
	buf[2] = 0x00
	buf[3] = atypIPv4
	copy(buf[4:8], ip4)
	binary.BigEndian.PutUint16(buf[8:10], port)
	_, err := w.Write(buf)
	return err
}

func parseUDPDatagram(pkt []byte) (*net.UDPAddr, []byte, error) {
	if len(pkt) < 10 {
		return nil, nil, errors.New("short socks5 udp packet")
	}
	if pkt[2] != 0x00 {
		// Fragmented UDP packets are not supported
		return nil, nil, errors.New("fragmented udp not supported")
	}
	atyp := pkt[3]
	offset := 4
	var host string
	switch atyp {
	case atypIPv4:
		if len(pkt) < offset+4+2 {
			return nil, nil, errors.New("short ipv4 udp header")
		}
		host = net.IP(pkt[offset : offset+4]).String()
		offset += 4
	case atypIPv6:
		if len(pkt) < offset+16+2 {
			return nil, nil, errors.New("short ipv6 udp header")
		}
		host = net.IP(pkt[offset : offset+16]).String()
		offset += 16
	case atypDomain:
		dlen := int(pkt[offset])
		offset++
		if len(pkt) < offset+dlen+2 {
			return nil, nil, errors.New("short domain udp header")
		}
		host = string(pkt[offset : offset+dlen])
		offset += dlen
	default:
		return nil, nil, errors.New("unsupported udp atyp")
	}
	port := int(binary.BigEndian.Uint16(pkt[offset : offset+2]))
	offset += 2

	udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, nil, err
	}
	return udpAddr, pkt[offset:], nil
}

func buildUDPDatagram(src *net.UDPAddr, payload []byte) []byte {
	v4 := src.IP.To4()
	if v4 == nil {
		return nil
	}
	hdr := make([]byte, 10+len(payload))
	hdr[0] = 0x00
	hdr[1] = 0x00
	hdr[2] = 0x00
	hdr[3] = atypIPv4
	copy(hdr[4:8], v4)
	binary.BigEndian.PutUint16(hdr[8:10], uint16(src.Port))
	copy(hdr[10:], payload)
	return hdr
}
