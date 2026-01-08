package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/Mattilsynet/h8s/pkg/h8sreverse"
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
}

var (
	NATSOptions     NATSConnectionOptions
	natsURLFlag     = flag.String("nats-url", "", "NATS server URL")
	natsCredsFlag   = flag.String("nats-creds", "", "Path to NATS credentials file (optional)")
	otelEnabledFlag = flag.Bool("otel-enabled", false, "Enable OpenTelemetry tracing and metrics")
	otelEndpoint    = flag.String("otel-endpoint", "", "")
	otel            = &othell.Othell{}
	hostnameFlag    = flag.String("hostname", "", "Public hostname to listen for (required)")
	backendURLFlag  = flag.String("backend-url", "", "URL of the local backend service (e.g. http://localhost:8080)")
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
		Name:            "h8srd",
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
		semconv.ServiceNameKey.String("h8srd"),
	}
	sres := resource.NewWithAttributes(
		semconv.SchemaURL,
		commonAttrs...,
	)

	var err error
	otel, err = othell.New(
		"h8srd",
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
	if *otelEnabledFlag {
		defer func() { _ = otel.MeterProvider.Shutdown(context.Background()) }()
		defer func() { _ = otel.TraceProvider.Shutdown(context.Background()) }()
	}

	nc, err := NATSConnect(NATSOptions)
	if err != nil {
		slog.Error("Failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

	slog.Info("Connected to NATS", "url", NATSOptions.URL)

	// Validate required flags
	if *hostnameFlag == "" {
		slog.Error("hostname flag is required")
		os.Exit(1)
	}

	// Create and start the reverse proxy
	proxy := h8sreverse.NewReverseProxy(nc)

	proxy.FilterHost = *hostnameFlag

	if *backendURLFlag != "" {
		u, err := url.Parse(*backendURLFlag)
		if err != nil {
			slog.Error("Invalid backend-url", "error", err)
			os.Exit(1)
		}
		proxy.BackendURL = u
		slog.Info("Configured backend", "url", u.String())
	}

	ctx := context.Background()
	if err := proxy.SubscribeForHost(ctx, *hostnameFlag); err != nil {
		slog.Error("Failed to subscribe", "error", err)
		os.Exit(1)
	}

	slog.Info("h8srd (Reverse Daemon) started", "hostname", *hostnameFlag)

	// Block forever (or until signal)
	select {}
}
