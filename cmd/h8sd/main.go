package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	h8s "github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/Mattilsynet/othell/pkg/othell"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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
	NATSOptions          NATSConnectionOptions
	natsURLFlag          = flag.String("nats-url", "", "NATS server URL")
	natsCredsFlag        = flag.String("nats-creds", "", "Path to NATS credentials file (optional)")
	trackInterestFlag    = flag.Bool("track-interest", false, "Enable Interest Tracker for self configuration. (Will no longer accept arbitrary request when enabled.)")
	otelEnabledFlag      = flag.Bool("otel-enabled", false, "Enable OpenTelemetry tracing and metrics")
	otelEndpoint         = flag.String("otel-endpoint", "", "")
	otel                 = &othell.Othell{}
	authorizationKeyFlag = flag.String("authorization-key", "", "Naive authorization key for all endpoints.")
	publishOnlyFlag      = flag.Bool("publish-only", false, "Enable publish-only mode, expect no reply's everyting will become 200 OK")
	maxBodySizeFlag      = flag.Int64("max-body-size", 2*1024*1024, "Max size of request body in bytes (default 2MB)")
	hostFiltersFlag      = flag.String("host-filters", "", "Comma separated list of allowed hosts (e.g. 'example.com,api.example.com')")
	requestTimeoutFlag   = flag.Duration("request-timeout", 30*time.Second, "Timeout for upstream requests")
	allowedOriginsFlag   = flag.String("allowed-origins", "", "Comma separated list of allowed WebSocket Origin values")
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
	if *otelEnabledFlag {
		defer func() { _ = otel.MeterProvider.Shutdown(context.Background()) }()
		defer func() { _ = otel.TraceProvider.Shutdown(context.Background()) }()
	}

	nc, err := NATSConnect(NATSOptions)
	if err != nil {
		slog.Error("Failed to connect to NATS", "error", err)
		os.Exit(1)
	}

	// Construct slice of Options for h8sproxy factory based on command line flags.
	var executionOptions []h8s.Option

	if *trackInterestFlag {
		var itOptions []tracker.InterestTrackerOption
		itOptions = append(itOptions, tracker.WithInterestSubject(h8s.H8SInterestControlSubject+".interest"))
		if *otelEnabledFlag {
			itOptions = append(itOptions, tracker.WithOTELMeter(otel.Meter))
		}

		h8stracker := tracker.NewInterestTracker(
			nc,
			itOptions...)

		executionOptions = append(executionOptions, h8s.WithInterestOnly())
		executionOptions = append(executionOptions, h8s.WithInterestTracker(h8stracker))
	}

	if *otelEnabledFlag {
		executionOptions = append(executionOptions, h8s.WithOTELMeter(otel.Meter))
		executionOptions = append(executionOptions, h8s.WithOTELTracer(otel.Tracer))
	}

	if len(*authorizationKeyFlag) > 0 {
		slog.Info("Naive authorization mode enabled")
		executionOptions = append(executionOptions, h8s.WithNaiveAuthorizationKey(*authorizationKeyFlag))
	}

	if *publishOnlyFlag {
		slog.Info("Running in publish only mode for http requests. All requests will get 200 OK.")
		executionOptions = append(executionOptions, h8s.WithPublishOnly())
	}

	if *maxBodySizeFlag > 0 {
		executionOptions = append(executionOptions, h8s.WithMaxBodySize(*maxBodySizeFlag))
	}

	if *hostFiltersFlag != "" {
		hosts := strings.Split(*hostFiltersFlag, ",")
		for _, host := range hosts {
			if trimmed := strings.TrimSpace(host); trimmed != "" {
				slog.Info("Adding host filter", "host", trimmed)
				executionOptions = append(executionOptions, h8s.WithHostFilter(trimmed))
			}
		}
	}

	if *allowedOriginsFlag != "" {
		origins := strings.Split(*allowedOriginsFlag, ",")
		var allowed []string
		for _, origin := range origins {
			if trimmed := strings.TrimSpace(origin); trimmed != "" {
				allowed = append(allowed, trimmed)
			}
		}
		if len(allowed) > 0 {
			slog.Info("Adding allowed WebSocket origins", "count", len(allowed))
			executionOptions = append(executionOptions, h8s.WithAllowedOrigins(allowed...))
		}
	}

	if *requestTimeoutFlag > 0 {
		executionOptions = append(executionOptions, h8s.WithRequestTimeout(*requestTimeoutFlag))
	}

	h8sproxy := h8s.NewH8Sproxy(
		nc,
		executionOptions...)

	mux := http.NewServeMux()
	mux.HandleFunc("/", h8sproxy.Handler)

	var muxhandler http.Handler
	if *otelEnabledFlag {
		muxhandler = otelhttp.NewHandler(mux, "h8s")
	} else {
		muxhandler = mux
	}
	slog.Info("Starting h8sd", "port", "8080")

	if err := http.ListenAndServe("0.0.0.0:8080", muxhandler); err != nil {
		slog.Error("Failed to start server", "error", err)
		os.Exit(1)
	}
}
