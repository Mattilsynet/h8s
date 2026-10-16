package h8sproxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// TLSMode selects how the h8sd HTTP server terminates TLS.
type TLSMode string

const (
	// TLSModeOff serves plain HTTP.
	TLSModeOff TLSMode = "off"
	// TLSModeStatic serves HTTPS using a certificate and key supplied as files.
	TLSModeStatic TLSMode = "static"
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
	case TLSModeStatic:
		slog.Info("Starting h8sd", "address", config.Address, "tls_mode", TLSModeStatic)
		// The certificate is already loaded into server.TLSConfig, so no
		// file paths are passed here.
		return server.ListenAndServeTLS("", "")
	default:
		return fmt.Errorf("invalid tls-mode %q: expected %q or %q", config.TLSMode, TLSModeOff, TLSModeStatic)
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
	default:
		return nil, fmt.Errorf("invalid tls-mode %q: expected %q or %q", config.TLSMode, TLSModeOff, TLSModeStatic)
	}
}
