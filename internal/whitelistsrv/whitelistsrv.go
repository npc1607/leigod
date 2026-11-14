// Package whitelistsrv 在宿主机上提供雷神 IP 白名单文件的本地 HTTPS 副本。
//
// 背景：雷神的加速引擎启动时会去 https://dfs01.nn.com/.../pcipwhite.txt 下载
// IP 白名单，下载失败会导致 “load the black white list failed” 并放弃初始化；
// 但白名单内容为空时，引擎会走兜底逻辑 “ip whitelist size is zero -> go acc”，
// 也就是全部流量一律加速。
//
// 因此本包生成一个自签 CA + 服务器证书，把该域名指向宿主机 veth 地址，并把
// 自签 CA 注入命名空间内的 CA bundle，从而在不改动雷神程序的前提下替换白名单内容。
package whitelistsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Paths 是证书与 CA bundle 的落盘位置。
type Paths struct {
	Dir      string
	CAFile   string
	CAKey    string
	CertFile string
	KeyFile  string
	Bundle   string
}

// ResolvePaths 计算 <dir> 下的证书文件路径。
func ResolvePaths(dir string) Paths {
	return Paths{
		Dir:      dir,
		CAFile:   filepath.Join(dir, "ca.crt"),
		CAKey:    filepath.Join(dir, "ca.key"),
		CertFile: filepath.Join(dir, "server.crt"),
		KeyFile:  filepath.Join(dir, "server.key"),
		Bundle:   filepath.Join(dir, "ca-bundle.crt"),
	}
}

// EnsureCerts 生成（或复用）自签 CA 与 dfs01 域名的服务器证书。
func EnsureCerts(dir, hostname string) (Paths, error) {
	p := ResolvePaths(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return p, err
	}
	if fileExists(p.CAFile) && fileExists(p.CAKey) && fileExists(p.CertFile) && fileExists(p.KeyFile) {
		return p, nil
	}

	// --- CA ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return p, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "leigod local CA", Organization: []string{"leigod-isolated"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return p, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return p, err
	}
	if err := writePEM(p.CAFile, "CERTIFICATE", caDER); err != nil {
		return p, err
	}
	if err := writeKeyPEM(p.CAKey, caKey); err != nil {
		return p, err
	}

	// --- 服务器证书 ---
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return p, err
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return p, err
	}
	if err := writePEM(p.CertFile, "CERTIFICATE", srvDER); err != nil {
		return p, err
	}
	if err := writeKeyPEM(p.KeyFile, srvKey); err != nil {
		return p, err
	}
	return p, nil
}

// BuildBundle 把自签 CA 追加到系统 CA bundle 之后，生成供命名空间使用的 bundle。
func BuildBundle(p Paths) error {
	caPEM, err := os.ReadFile(p.CAFile)
	if err != nil {
		return err
	}
	var base []byte
	for _, candidate := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/cert.pem",
	} {
		if data, err := os.ReadFile(candidate); err == nil {
			base = data
			break
		}
	}
	out := make([]byte, 0, len(base)+len(caPEM)+64)
	out = append(out, base...)
	if len(base) > 0 && base[len(base)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, caPEM...)
	return os.WriteFile(p.Bundle, out, 0o644)
}

// Server 提供白名单内容。
type Server struct {
	Listen   string
	Body     []byte
	CertFile string
	KeyFile  string
	Logf     func(string, ...any)
}

// Start 在 ctx 取消前持续服务（HTTPS）。
func (s *Server) Start(ctx context.Context) error {
	cert, err := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
	if err != nil {
		return fmt.Errorf("加载证书失败: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(s.Body)))
		_, _ = w.Write(s.Body)
	})

	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.Listen, err)
	}
	srv := &http.Server{
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if s.Logf != nil {
		s.Logf("白名单 HTTPS 服务已启动: https://%s (内容 %d 字节)", s.Listen, len(s.Body))
	}
	if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

func writePEM(path, typ string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644)
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}
