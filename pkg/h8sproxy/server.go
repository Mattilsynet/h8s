package h8sproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"time"
)

// TLSMode selects how the h8sd HTTP server terminates TLS.
type TLSMode string

const (
	// TLSModeOff serves plain HTTP.
	TLSModeOff TLSMode = "off"
	// TLSModeStatic serves HTTPS using a certificate and key supplied as files.
	TLSModeStatic TLSMode = "static"
	// TLSModeSelfSigned serves HTTPS using a self-signed certificate generated
	// in memory at startup. Intended for local development.
	TLSModeSelfSigned TLSMode = "selfsigned"
)

// ServerConfig configures the h8sd HTTP server.
type ServerConfig struct {
	// Address is the listen address, e.g. "0.0.0.0:8080".
	Address string
	// TLSMode selects the TLS behavior. Defaults to TLSModeOff when empty.
	TLSMode TLSMode
	// CertFile is the path to the TLS certificate file (PEM).
	// Required when TLSMode is TLSModeStatic.
	CertFile string
	// KeyFile is the path to the TLS private-key file (PEM).
	// Required when TLSMode is TLSModeStatic.
	KeyFile string
	// HostFilters are the allowed hosts. In self-signed mode they are added as
	// SANs on the generated certificate.
	HostFilters []string
}

// ListenAndServe starts an HTTP or HTTPS server for handler using config.
// In static mode the certificate/key pair is loaded and validated before the
// listener is opened, so misconfiguration fails startup.
func ListenAndServe(handler http.Handler, config ServerConfig) error {
	tlsConfig, err := loadTLSConfig(config)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:      config.Address,
		Handler:   handler,
		TLSConfig: tlsConfig,
	}

	switch config.TLSMode {
	case TLSModeOff, "":
		slog.Info("Starting h8sd", "address", config.Address, "tls_mode", TLSModeOff)
		return server.ListenAndServe()
	case TLSModeStatic, TLSModeSelfSigned:
		slog.Info("Starting h8sd", "address", config.Address, "tls_mode", config.TLSMode)
		// The certificate is already loaded into server.TLSConfig, so no
		// file paths are passed here.
		return server.ListenAndServeTLS("", "")
	default:
		return fmt.Errorf("invalid tls-mode %q: expected %q, %q or %q", config.TLSMode, TLSModeOff, TLSModeStatic, TLSModeSelfSigned)
	}
}

func loadTLSConfig(config ServerConfig) (*tls.Config, error) {
	switch config.TLSMode {
	case TLSModeOff, "":
		return nil, nil
	case TLSModeStatic:
		if config.CertFile == "" {
			return nil, errors.New("tls-cert is required when tls-mode is static")
		}
		if config.KeyFile == "" {
			return nil, errors.New("tls-key is required when tls-mode is static")
		}

		certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}

		return &tls.Config{
			Certificates: []tls.Certificate{certificate},
		}, nil
	case TLSModeSelfSigned:
		certificate, err := generateSelfSignedCert(config.HostFilters)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			Certificates: []tls.Certificate{*certificate},
		}, nil
	default:
		return nil, fmt.Errorf("invalid tls-mode %q: expected %q, %q or %q", config.TLSMode, TLSModeOff, TLSModeStatic, TLSModeSelfSigned)
	}
}

// generateSelfSignedCert creates an in-memory ECDSA key and a self-signed
// certificate valid for localhost, loopback IPs, and the given hosts. Hosts
// are normalized (ports and brackets stripped) before being added as SANs.
func generateSelfSignedCert(hosts []string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate self-signed key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("generate self-signed serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "h8sd self-signed"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	seen := map[string]bool{"localhost": true}
	for _, host := range hosts {
		host = normalizeHost(host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create self-signed certificate: %w", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}
