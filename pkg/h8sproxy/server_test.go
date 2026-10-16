package h8sproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadTLSConfig(t *testing.T) {
	t.Run("off returns nil config", func(t *testing.T) {
		cfg, err := loadTLSConfig(ServerConfig{TLSMode: TLSModeOff})
		if err != nil {
			t.Fatalf("loadTLSConfig() error = %v", err)
		}
		if cfg != nil {
			t.Fatalf("loadTLSConfig() = %v, want nil", cfg)
		}
	})

	t.Run("empty mode defaults to off", func(t *testing.T) {
		cfg, err := loadTLSConfig(ServerConfig{})
		if err != nil {
			t.Fatalf("loadTLSConfig() error = %v", err)
		}
		if cfg != nil {
			t.Fatalf("loadTLSConfig() = %v, want nil", cfg)
		}
	})

	t.Run("static requires cert", func(t *testing.T) {
		_, err := loadTLSConfig(ServerConfig{TLSMode: TLSModeStatic, KeyFile: "key.pem"})
		if err == nil {
			t.Fatal("loadTLSConfig() expected error for missing cert")
		}
	})

	t.Run("static requires key", func(t *testing.T) {
		_, err := loadTLSConfig(ServerConfig{TLSMode: TLSModeStatic, CertFile: "cert.pem"})
		if err == nil {
			t.Fatal("loadTLSConfig() expected error for missing key")
		}
	})

	t.Run("static loads valid key pair", func(t *testing.T) {
		certFile, keyFile := writeTestKeyPair(t)
		cfg, err := loadTLSConfig(ServerConfig{
			TLSMode:  TLSModeStatic,
			CertFile: certFile,
			KeyFile:  keyFile,
		})
		if err != nil {
			t.Fatalf("loadTLSConfig() error = %v", err)
		}
		if cfg == nil {
			t.Fatal("loadTLSConfig() = nil, want config")
		}
		if len(cfg.Certificates) != 1 {
			t.Fatalf("loadTLSConfig() certificates = %d, want 1", len(cfg.Certificates))
		}
	})

	t.Run("static rejects invalid cert file", func(t *testing.T) {
		certFile := filepath.Join(t.TempDir(), "cert.pem")
		if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadTLSConfig(ServerConfig{
			TLSMode:  TLSModeStatic,
			CertFile: certFile,
			KeyFile:  certFile,
		})
		if err == nil {
			t.Fatal("loadTLSConfig() expected error for invalid cert")
		}
	})

	t.Run("static rejects mismatched key pair", func(t *testing.T) {
		certFile, _ := writeTestKeyPair(t)
		_, otherKey := writeTestKeyPair(t)
		_, err := loadTLSConfig(ServerConfig{
			TLSMode:  TLSModeStatic,
			CertFile: certFile,
			KeyFile:  otherKey,
		})
		if err == nil {
			t.Fatal("loadTLSConfig() expected error for mismatched key")
		}
	})

	t.Run("invalid mode returns error", func(t *testing.T) {
		_, err := loadTLSConfig(ServerConfig{TLSMode: TLSMode("bogus")})
		if err == nil {
			t.Fatal("loadTLSConfig() expected error for invalid mode")
		}
	})
}

func TestLoadTLSConfigSelfSigned(t *testing.T) {
	t.Run("generates a certificate", func(t *testing.T) {
		cfg, err := loadTLSConfig(ServerConfig{TLSMode: TLSModeSelfSigned})
		if err != nil {
			t.Fatalf("loadTLSConfig() error = %v", err)
		}
		if cfg == nil {
			t.Fatal("loadTLSConfig() = nil, want config")
		}
		if len(cfg.Certificates) != 1 {
			t.Fatalf("loadTLSConfig() certificates = %d, want 1", len(cfg.Certificates))
		}
	})

	t.Run("ignores cert and key flags", func(t *testing.T) {
		cfg, err := loadTLSConfig(ServerConfig{
			TLSMode:  TLSModeSelfSigned,
			CertFile: "ignored.pem",
			KeyFile:  "ignored.pem",
		})
		if err != nil {
			t.Fatalf("loadTLSConfig() error = %v", err)
		}
		if len(cfg.Certificates) != 1 {
			t.Fatalf("loadTLSConfig() certificates = %d, want 1", len(cfg.Certificates))
		}
	})

	t.Run("includes host filters as SANs", func(t *testing.T) {
		cfg, err := loadTLSConfig(ServerConfig{
			TLSMode:     TLSModeSelfSigned,
			HostFilters: []string{"api.example.com", "app.example.com:8443", "127.0.0.1"},
		})
		if err != nil {
			t.Fatalf("loadTLSConfig() error = %v", err)
		}

		der := cfg.Certificates[0].Certificate[0]
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse certificate: %v", err)
		}

		dns := map[string]bool{}
		for _, name := range cert.DNSNames {
			dns[name] = true
		}
		for _, want := range []string{"localhost", "api.example.com", "app.example.com"} {
			if !dns[want] {
				t.Errorf("certificate DNSNames missing %q (have %v)", want, cert.DNSNames)
			}
		}

		ips := map[string]bool{}
		for _, ip := range cert.IPAddresses {
			ips[ip.String()] = true
		}
		for _, want := range []string{"127.0.0.1", "::1"} {
			if !ips[want] {
				t.Errorf("certificate IPAddresses missing %q (have %v)", want, cert.IPAddresses)
			}
		}
	})
}

func TestListenAndServeTLS(t *testing.T) {
	certFile, keyFile := writeTestKeyPair(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("request did not arrive over TLS")
		}
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	done := make(chan error, 1)
	go func() {
		done <- ListenAndServe(handler, ServerConfig{
			Address:  addr,
			TLSMode:  TLSModeStatic,
			CertFile: certFile,
			KeyFile:  keyFile,
		})
	}()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = client.Get("https://" + addr)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	select {
	case err := <-done:
		t.Fatalf("ListenAndServe returned unexpectedly: %v", err)
	default:
	}
}

func TestListenAndServeSelfSigned(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("request did not arrive over TLS")
		}
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	done := make(chan error, 1)
	go func() {
		done <- ListenAndServe(handler, ServerConfig{
			Address:     addr,
			TLSMode:     TLSModeSelfSigned,
			HostFilters: []string{"api.example.com"},
		})
	}()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = client.Get("https://" + addr)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	select {
	case err := <-done:
		t.Fatalf("ListenAndServe returned unexpectedly: %v", err)
	default:
	}
}

// writeTestKeyPair generates a self-signed certificate and key, writes them to
// temporary files, and returns their paths.
func writeTestKeyPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
