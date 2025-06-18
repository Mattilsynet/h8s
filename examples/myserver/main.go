package main

import (
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
		h8sservice.WithInterestPublishSubject("h8s.control.interest"))
	svc.AddRequestService("localhost", "/echo", "POST", myHandler{})

	go svc.Run()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down")
	log.Println("bye")
}

type myHandler struct{}

func (myHandler) Handle(r micro.Request) {
	r.Respond(r.Data())
}
