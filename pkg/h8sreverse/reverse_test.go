package h8sreverse

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// fakeTransport records the request and returns a static response.
type fakeTransport struct{ req *http.Request }

func (t *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.req = r
	body := io.NopCloser(strings.NewReader("pong"))
	return &http.Response{StatusCode: 200, Body: body}, nil
}

func startEmbeddedNATS(t *testing.T) *server.Server {
	opts := &server.Options{Port: 4223}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	if !ns.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	t.Cleanup(func() { ns.Shutdown() })
	return ns
}

func TestReverseProxySubscribeAll(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	rp := NewReverseProxy(nc)
	rp.client = &http.Client{Transport: &fakeTransport{}}
	err = rp.SubscribeAll(context.Background())
	require.NoError(t, err)

	subject := subjectmapper.SubjectPrefix + ".http.GET.localhost.ping"
	msg := &nats.Msg{Subject: subject, Reply: "_INBOX.reply1", Data: []byte("hello")}

	replySub, err := nc.SubscribeSync(msg.Reply)
	require.NoError(t, err)
	err = nc.PublishMsg(msg)
	require.NoError(t, err)

	reply, err := replySub.NextMsgWithContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, []byte("pong"), reply.Data)

	req := rp.client.Transport.(*fakeTransport).req
	require.NotNil(t, req)
	require.Equal(t, "GET", req.Method)
	require.Equal(t, "localhost", req.Host)
	require.Equal(t, "http://localhost/ping", req.URL.String())
}
