package main

import (
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
	defer nc.Drain()

	svc := h8sservice.NewService(
		nc,
		h8sservice.WithInterestPublishSubject(h8sproxy.H8SInterestControlSubject))
	svc.AddRequestHandler("localhost", "/echo", "POST", myHandler{})
	svc.AddRequestHandler("localhost", "/html", "GET", myHTMLHandler{})
	go svc.Run()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	svc.Shutdown()
	log.Println("shutting down")
	log.Println("bye")
}

type myHandler struct{}

func (myHandler) Handle(r micro.Request) {
	r.Respond(r.Data())
}

type myHTMLHandler struct{}

func (myHTMLHandler) Handle(r micro.Request) {
	r.Respond([]byte("<html><body><h1>Hello, World!</h1></body></html>"))
}
