package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quackmux/internal/config"
)

// selfSigned writes a self-signed P-256 certificate for localhost like the
// one the openssl command of docs/tls.md makes (CA:TRUE, PKCS#8 key) and
// returns the server.tls settings for it and its DER bytes.
func selfSigned(t *testing.T) (*config.TLSConfig, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tc := &config.TLSConfig{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	for path, block := range map[string]*pem.Block{
		tc.CertFile: {Type: "CERTIFICATE", Bytes: der},
		tc.KeyFile:  {Type: "PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return tc, der
}

// TestServeTLS runs a listener the way serve starts the Quack port with
// server.tls: HTTPS from TLS 1.2 on with the configured certificate, whose
// SHA-256 is the fingerprint that clients pin, and an answer "400 Bad
// Request" to plain HTTP, which a DuckDB client with DISABLE_SSL true (or one
// that attaches localhost without a pin) reports as "Bad Request"
// (docs/tls.md).
func TestServeTLS(t *testing.T) {
	tc, der := selfSigned(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	log := slog.New(slog.DiscardHandler)
	srv := &http.Server{
		Addr:     addr,
		Handler:  http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "quack") }),
		ErrorLog: serverErrorLog(log),
	}
	served := make(chan error, 1)
	go func() { served <- serve(srv, tc, "quack", log) }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = srv.Close()
		}
	})

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	clientTLS := func(minVersion, maxVersion uint16) *tls.Config {
		return &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: minVersion, MaxVersion: maxVersion}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS(tls.VersionTLS12, 0)}}
	defer client.CloseIdleConnections()
	var resp *http.Response
	for deadline := time.Now().Add(10 * time.Second); ; {
		if resp, err = client.Get("https://" + addr + "/"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no HTTPS answer: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "quack" {
		t.Fatalf("HTTPS answer %q (%v), want the handler's", body, err)
	}
	if got, want := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw), sha256.Sum256(der); got != want {
		t.Errorf("the listener presents a certificate with fingerprint %X, want %X (the configured one)", got, want)
	}

	// TLS 1.2 is enough, TLS 1.1 is refused.
	for _, c := range []struct {
		name     string
		cfg      *tls.Config
		accepted bool
	}{
		{"TLS 1.2", clientTLS(tls.VersionTLS12, tls.VersionTLS12), true},
		{"TLS 1.1", clientTLS(tls.VersionTLS10, tls.VersionTLS11), false},
	} {
		conn, err := tls.Dial("tcp", addr, c.cfg)
		if err == nil {
			_ = conn.Close()
		}
		if (err == nil) != c.accepted {
			t.Errorf("%s handshake: err %v, want accepted %v", c.name, err, c.accepted)
		}
	}

	plain, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("plain HTTP request: %v", err)
	}
	body, _ = io.ReadAll(plain.Body)
	plain.Body.Close()
	if plain.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "HTTP request to an HTTPS server") {
		t.Errorf("plain HTTP got %s %q, want 400 Bad Request", plain.Status, body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if err := <-served; err != nil {
		t.Errorf("serve returned %v after Shutdown, want nil", err)
	}
}

// TestServeTLSMissingCertificate: a certificate file that cannot be read
// stops the listener with an error that names it.
func TestServeTLSMissingCertificate(t *testing.T) {
	dir := t.TempDir()
	tc := &config.TLSConfig{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	err := serve(srv, tc, "quack", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "quack listener") || !strings.Contains(err.Error(), "tls.crt") {
		t.Fatalf("serve: %v, want an error of the quack listener that names tls.crt", err)
	}
}
