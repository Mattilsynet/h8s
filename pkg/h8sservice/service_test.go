package h8sservice

import (
	"context"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/internal/natstest"
	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"github.com/stretchr/testify/require"
)

type myTestHandler struct{}

func (myTestHandler) Handle(r micro.Request) {
	r.Respond([]byte("ok"))
}

func TestServiceDefaultsInterestPublishSubject(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	client := NewService(nc)
	client.AddRequestHandler("localhost", "/default", "GET", "http", myTestHandler{})

	sub, err := nc.SubscribeSync(h8sproxy.H8SInterestControlSubject)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	client.ctx = ctx
	go client.Run()

	_, err = sub.NextMsg(2 * time.Second)
	require.NoError(t, err)

	cancel()
	client.Shutdown()
}

func TestAddRequestServiceAndHandlerInvoke(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Drain()

	client := NewService(nc, WithInterestPublishSubject("h8s.control.interest"))

	called := make(chan bool, 1)

	host := "localhost"
	path := "/testpath"
	client.AddRequestHandler(host, path, "GET", "https", myTestHandler{})

	// Prepare cancelable context
	ctx, cancel := context.WithCancel(context.Background())
	client.ctx = ctx

	// Run client in background
	go client.Run()

	// Wait a bit to ensure the service is added
	time.Sleep(500 * time.Millisecond)

	// Publish a request matching the service's subject

	subject := subjectmapper.NewSubjectMap(subjectmapper.HTTPReqFromArgs("https", host, path, "GET")).PublishSubject()
	t.Logf("Using subject: %s", subject)
	resp, err := nc.Request(subject, []byte("test"), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to send request: %v", err)
	}
	if string(resp.Data) != "ok" {
		t.Errorf("unexpected response: %s", resp.Data)
	}
	called <- true

	// Verify the handler was called
	select {
	case <-called:
		// OK
	case <-time.After(time.Second):
		t.Errorf("handler was not called")
	}

	// Clean up
	cancel()
	time.Sleep(200 * time.Millisecond) // allow shutdown
}
