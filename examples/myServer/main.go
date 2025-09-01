package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/h8sservice"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

func main() {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		slog.Error("unable to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer func() {
		slog.Info("draining NATS connection")
		nc.Drain()
	}()

	// Root context with cancellation on signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, initiating shutdown", "signal", sig)
		cancel()

		// Force exit if second signal is received
		sig = <-sigCh
		slog.Warn("second signal received, forcing exit", "signal", sig)
		os.Exit(1)
	}()

	svc := h8sservice.NewService(
		nc,
		h8sservice.WithInterestPublishSubject(h8sproxy.H8SInterestControlSubject),
	)
	svc.AddRequestHandler("localhost", "/echo", "POST", "https", myHandler{})
	svc.AddRequestHandler("localhost", "/html", "GET", "https", myHTMLHandler{})

	wss := &websocketThingy{
		Ctx: ctx,
	}
	wsh := h8sservice.NewWebsocketHandler(wss, "localhost", "/ws")
	wss.WebsocketHandler = wsh
	svc.RegisterWebsocketHandler(wsh)

	// Run the h8sservice
	go svc.Run()
	go wss.Writer()

	// Block until context is canceled
	<-ctx.Done()

	slog.Info("shutting down service")
	svc.Shutdown()
	slog.Info("bye")
}

type myHandler struct{}

func (myHandler) Handle(r micro.Request) {
	r.Respond(r.Data())
}

type myHTMLHandler struct{}

func (myHTMLHandler) Handle(r micro.Request) {
	r.Respond([]byte("<html><body><h1>Hello, World!</h1></body></html>"))
}

// TODO: Need to better construct your websocket struct.

type websocketThingy struct {
	Ctx              context.Context
	WebsocketHandler *h8sservice.WebsocketHandler
}

func (w *websocketThingy) Read(msg *nats.Msg) {
	// Handle incoming websocket messages
	slog.Info("Received message: %s", "message", msg.Data)
}

func (w *websocketThingy) Writer() {
	slog.Info("running websocket writer")
	select {
	case <-w.Ctx.Done():
		slog.Info("WebSocket writer shutting down")
		return
	default:
		for {
			for _, conn := range w.WebsocketHandler.GetWSConnsBySubject(w.WebsocketHandler.ReceiveSubject) {
				slog.Info("connection:", "conn", conn)
				payload := []byte("Hello from WebSocket server!")
				w.WebsocketHandler.NatsConn.Publish(conn, payload)
			}
			// Optional: small sleep to prevent 100% CPU if idle
			time.Sleep(1 * time.Second)
		}
	}
}
