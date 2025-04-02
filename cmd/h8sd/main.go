package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	h8s "github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/nats-io/nats.go"
)

type NATSConnectionOptions struct {
	URL             string
	CredsPath       string
	Timeout         time.Duration
	Name            string
	ReconnectBuffer int
	InboxPrefix     string
}

var NATSOptions NATSConnectionOptions

func NATSConnect(opts NATSConnectionOptions) (*nats.Conn, error) {
	natsOpts := []nats.Option{
		nats.Timeout(opts.Timeout),
		nats.Name(opts.Name),
		nats.ReconnectBufSize(opts.ReconnectBuffer),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1), // retry forever
	}

	if opts.CredsPath != "" {
		natsOpts = append(natsOpts, nats.UserCredentials(opts.CredsPath))
	}

	nc, err := nats.Connect(opts.URL, natsOpts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect failed: %w", err)
	}
	return nc, nil
}

func init() {
	handler := slog.NewJSONHandler(os.Stdout, nil)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	NATSOptions = NATSConnectionOptions{
		URL:             "nats://localhost:4222",
		Name:            "h8sd",
		Timeout:         5 * time.Second,
		CredsPath:       "",
		ReconnectBuffer: 8388608,
	}

	if len(os.Getenv("NATS_URL")) > 0 {
		NATSOptions.URL = os.Getenv("NATS_URL")
	}
	if len(os.Getenv("NATS_CREDS_PATH")) > 0 {
		NATSOptions.CredsPath = os.Getenv("NATS_CREDS_PATH")
	}
}

func main() {
	mux := http.NewServeMux()
	nc, err := NATSConnect(NATSOptions)
	if err != nil {
		slog.Error("Failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	h8sproxy := h8s.NewH8Sproxy(nc)

	mux.HandleFunc("/", h8sproxy.Handler)

	slog.Info("Staring k8sd", "port", "8080")
	if err := http.ListenAndServe("0.0.0.0:8080", mux); err != nil {
		slog.Error("Failed to start server:", "error", err)
	}
}
