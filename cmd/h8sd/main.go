package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	h8s "github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/Mattilsynet/othell/pkg/othell"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
)

type NATSConnectionOptions struct {
	URL             string
	CredsPath       string
	Timeout         time.Duration
	Name            string
	ReconnectBuffer int
	InboxPrefix     string
}

var (
	NATSOptions     NATSConnectionOptions
	natsURLFlag     = flag.String("nats-url", "", "NATS server URL")
	natsCredsFlag   = flag.String("nats-creds", "", "Path to NATS credentials file (optional)")
	otelEnabledFlag = flag.Bool("otel-enabled", false, "Enable OpenTelemetry tracing and metrics")
	otelEndpoint    = flag.String("otel-endpoint", "", "")
	otel            = &othell.Othell{}
)

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

	flag.Parse()

	// Determine NATS URL
	url := *natsURLFlag
	if url == "" {
		url = os.Getenv("NATS_URL")
	}
	if url == "" {
		url = "nats://0.0.0.0:4222"
	}

	// Determine NATS Creds Path
	creds := *natsCredsFlag
	if creds == "" {
		creds = os.Getenv("NATS_CREDS_PATH")
	}

	NATSOptions = NATSConnectionOptions{
		URL:             url,
		CredsPath:       creds,
		Name:            "h8sd",
		Timeout:         5 * time.Second,
		ReconnectBuffer: 8 * 1024 * 1024, // 8MB
	}
}

func enableOTEL() {
	if !*otelEnabledFlag {
		slog.Info("OpenTelemetry tracing and metrics are disabled")
		return
	}
	slog.Info("Enabling OpenTelemetry tracing and metrics")

	commonAttrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(""),
		attribute.String("somekey", "somevalue"),
	}
	sres := resource.NewWithAttributes(
		semconv.SchemaURL,
		commonAttrs...,
	)

	var err error
	otel, err = othell.New(
		"h8sd",
		othell.WithCollectorEndpoint(*otelEndpoint),
		othell.WithResource(sres))
	if err != nil {
		slog.Error("Unable to initialize Othell", "error", err)
		os.Exit(1)
	}

	slog.Info("OpenTelemetry (Othell) initialized successfully")
}

func main() {
	enableOTEL()

	mux := http.NewServeMux()

	nc, err := NATSConnect(NATSOptions)
	if err != nil {
		slog.Error("Failed to connect to NATS", "error", err)
		os.Exit(1)
	}

	h8stracker := tracker.NewInterestTracker(nc, h8s.H8SControlSubjectPrefix+".interest")

	h8sproxy := h8s.NewH8Sproxy(
		nc,
		h8s.WithInterestOnly(),
		h8s.WithInterestTracker(h8stracker),
		h8s.WithOTELMeter(otel.Meter),
		h8s.WithOTELTracer(otel.Tracer))

	mux.HandleFunc("/", h8sproxy.Handler)

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			slog.Info(
				"number of websocket connections",
				"connections", h8sproxy.WSPool.ActiveConnections())
		}
	}()

	slog.Info("Starting h8sd", "port", "8080")
	if err := http.ListenAndServe("0.0.0.0:8080", mux); err != nil {
		slog.Error("Failed to start server", "error", err)
	}
}
