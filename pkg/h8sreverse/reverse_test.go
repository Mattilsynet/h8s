package h8sreverse

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// fakeTransport records the request and returns a static response.
type fakeTransport struct{ req *http.Request }

func (t *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.req = r
	body := io.NopCloser(strings.NewReader("pong"))
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"X-Test-Header": []string{"test-value"}},
		Body:       body,
	}, nil
}

func startEmbeddedNATS(t *testing.T) *server.Server {
	opts := &server.Options{Port: -1} // Dynamic port allocation to avoid conflicts
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
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
	require.Equal(t, "200", reply.Header.Get("Status-Code"))
	require.Equal(t, "200", reply.Header.Get("Status"))
	require.Equal(t, "test-value", reply.Header.Get("X-Test-Header"))

	req := rp.client.Transport.(*fakeTransport).req
	require.NotNil(t, req)
	require.Equal(t, "GET", req.Method)
	require.Equal(t, "localhost", req.Host)
	require.Equal(t, "http://localhost/ping", req.URL.String())
}

func TestRootPath(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	rp := NewReverseProxy(nc)
	rp.client = &http.Client{Transport: &fakeTransport{}}
	err = rp.SubscribeAll(context.Background())
	require.NoError(t, err)

	// Subject with 4 parts: h8s.http.GET.localhost (no path)
	// Actually subjectmapper usually produces h8s.http.GET.localhost. for root?
	// The code handles len(parts)==4.
	// h8s.http.GET.localhost -> parts: [h8s, http, GET, localhost] (len 4)
	subject := subjectmapper.SubjectPrefix + ".http.GET.localhost"
	msg := &nats.Msg{Subject: subject, Reply: "_INBOX.root", Data: []byte("")}

	replySub, err := nc.SubscribeSync(msg.Reply)
	require.NoError(t, err)
	err = nc.PublishMsg(msg)
	require.NoError(t, err)

	reply, err := replySub.NextMsgWithContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, "200", reply.Header.Get("Status-Code"))

	req := rp.client.Transport.(*fakeTransport).req
	require.Equal(t, "http://localhost/", req.URL.String())
}

// errorTransport returns an error on RoundTrip
type errorTransport struct{}

// mockResolver implements BackendResolver for testing
type mockResolver struct {
	ResolveFunc func(ctx context.Context, host, path string) (*url.URL, error)
}

func (m *mockResolver) Resolve(ctx context.Context, host, path string) (*url.URL, error) {
	if m.ResolveFunc != nil {
		return m.ResolveFunc(ctx, host, path)
	}
	return nil, nil
}

func TestReverseProxy_DynamicResolver(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	rp := NewReverseProxy(nc)

	// Create a mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "dynamic")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("dynamic response"))
	}))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)

	// Configure resolver to point to mock backend for specific host
	rp.Resolver = &mockResolver{
		ResolveFunc: func(ctx context.Context, host, path string) (*url.URL, error) {
			// Proxy logic splits subject by dot. "dynamic_local" is a single token, so it works.
			if host == "dynamic_local" {
				return backendURL, nil
			}
			return nil, nil
		},
	}

	// Subscribe
	err = rp.SubscribeAll(context.Background())
	require.NoError(t, err)

	// 1. Test Match
	// Use underscore to make it a single token in NATS subject
	subject := subjectmapper.SubjectPrefix + ".http.GET.dynamic_local.foo"
	msg := &nats.Msg{Subject: subject, Reply: "_INBOX.dyn1", Data: []byte("")}

	replySub, err := nc.SubscribeSync(msg.Reply)
	require.NoError(t, err)
	err = nc.PublishMsg(msg)
	require.NoError(t, err)

	reply, err := replySub.NextMsgWithContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, "200", reply.Header.Get("Status-Code"))
	require.Equal(t, "dynamic", reply.Header.Get("X-Backend"))
	require.Equal(t, "dynamic response", string(reply.Data))

	// 2. Test No Match (should fail or default)
	// If resolver returns nil, reverse proxy currently falls back to direct connection
	// which will fail for a fake host "unknown.local" in this environment.
	subject2 := subjectmapper.SubjectPrefix + ".http.GET.unknown.local.foo"
	msg2 := &nats.Msg{Subject: subject2, Reply: "_INBOX.dyn2", Data: []byte("")}

	replySub2, err := nc.SubscribeSync(msg2.Reply)
	require.NoError(t, err)
	err = nc.PublishMsg(msg2)
	require.NoError(t, err)

	reply2, err := replySub2.NextMsgWithContext(context.Background())
	require.NoError(t, err)
	// Expect 502 because unknown.local doesn't resolve/exist
	require.Equal(t, "502", reply2.Header.Get("Status-Code"))
}

func (t *errorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestErrorHandling(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	rp := NewReverseProxy(nc)
	rp.client = &http.Client{Transport: &errorTransport{}}
	err = rp.SubscribeAll(context.Background())
	require.NoError(t, err)

	subject := subjectmapper.SubjectPrefix + ".http.GET.localhost.fail"
	msg := &nats.Msg{Subject: subject, Reply: "_INBOX.fail", Data: []byte("")}

	replySub, err := nc.SubscribeSync(msg.Reply)
	require.NoError(t, err)
	err = nc.PublishMsg(msg)
	require.NoError(t, err)

	reply, err := replySub.NextMsgWithContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, "502", reply.Header.Get("Status-Code"))
	require.Equal(t, "502", reply.Header.Get("Status"))
	require.Contains(t, string(reply.Data), io.ErrUnexpectedEOF.Error())
}

var upgrader = websocket.Upgrader{}

func TestWebSocketProxy(t *testing.T) {
	// 1. Start a Mock WebSocket Backend
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer c.Close()
		for {
			mt, message, err := c.ReadMessage()
			if err != nil {
				break
			}
			// Echo back
			err = c.WriteMessage(mt, message)
			if err != nil {
				break
			}
		}
	})
	wsServer := httptest.NewServer(wsHandler)
	defer wsServer.Close()

	// Parse the port from the mock server URL
	// mock server URL is like http://127.0.0.1:45678
	// We need 127.0.0.1:45678 for the Host header
	host := strings.TrimPrefix(wsServer.URL, "http://")

	// 2. Start NATS and Reverse Proxy
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	rp := NewReverseProxy(nc)
	err = rp.SubscribeAll(context.Background())
	require.NoError(t, err)

	// 3. Simulate "Connection Established" control message
	replySubject := "_INBOX.client1"
	publishSubject := "h8s.ws.ws.localhost.ws"

	msg := &nats.Msg{
		Subject: "h8s.control.ws.conn.established",
		Reply:   replySubject,
		Header:  nats.Header{},
	}
	msg.Header.Set("X-H8s-PublishSubject", publishSubject)
	msg.Header.Set("Host", host)
	msg.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	msg.Header.Set("Connection", "Upgrade")
	msg.Header.Set("Upgrade", "websocket")
	msg.Header.Set("Sec-WebSocket-Version", "13")

	// Subscribe to what the proxy will output (messages from backend)
	sub, err := nc.SubscribeSync(replySubject)
	require.NoError(t, err)

	err = nc.PublishMsg(msg)
	require.NoError(t, err)

	// Wait for connection to be established (async)
	time.Sleep(100 * time.Millisecond)

	// 4. Send Data: Client -> Proxy (Data Subject) -> Backend -> Proxy (Reply Subject) -> Client
	// Send "Hello" to the data subject
	dataSubject := publishSubject
	dataMsg := &nats.Msg{
		Subject: dataSubject,
		Reply:   replySubject,
		Data:    []byte("Hello WebSocket"),
	}
	err = nc.PublishMsg(dataMsg)
	require.NoError(t, err)

	// 5. Receive Echo
	echoMsg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	require.Equal(t, "Hello WebSocket", string(echoMsg.Data))

	// 6. Close Connection
	closeMsg := &nats.Msg{
		Subject: "h8s.control.ws.conn.closed",
		Reply:   replySubject,
	}
	err = nc.PublishMsg(closeMsg)
	require.NoError(t, err)
}

func TestSSE(t *testing.T) {
	// 1. Start a Mock SSE Backend
	sseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// No Content-Length

		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: event %d\n\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	sseServer := httptest.NewServer(sseHandler)
	defer sseServer.Close()

	// 2. Setup NATS and Proxy
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	rp := NewReverseProxy(nc)
	// Must use real http client that allows following the SSE stream request?
	// Default client is fine.
	err = rp.SubscribeAll(context.Background())
	require.NoError(t, err)

	// 3. Send Request
	replySubject := "_INBOX.sse_client"

	port := strings.Split(sseServer.URL, ":")[2]

	msg := &nats.Msg{
		Subject: subjectmapper.SubjectPrefix + ".http.GET." + "127_0_0_1_" + port + ".stream",
		Reply:   replySubject,
		Data:    []byte(""),
	}

	rp.client = &http.Client{
		Transport: &pathRewriteTransport{Target: sseServer.URL},
	}

	sub, err := nc.SubscribeSync(replySubject)
	require.NoError(t, err)

	err = nc.PublishMsg(msg)
	require.NoError(t, err)

	// 4. Verify Chunks

	msg1, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	// Check headers
	require.Equal(t, "200", msg1.Header.Get("Status-Code"))
	require.Equal(t, "text/event-stream", msg1.Header.Get("Content-Type"))
	require.Equal(t, "", msg1.Header.Get("Content-Length")) // Should be removed
	require.Contains(t, string(msg1.Data), "data: event 1")

	// Msg 2
	msg2, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	require.Contains(t, string(msg2.Data), "data: event 2")
	require.Empty(t, msg2.Header) // No headers on subsequent chunks

	// Msg 3
	msg3, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	require.Contains(t, string(msg3.Data), "data: event 3")

	// Msg 4: Empty termination
	msg4, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	require.Empty(t, msg4.Data)
}

type pathRewriteTransport struct {
	Target string
}

func (t *pathRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite URL to target
	u, _ := url.Parse(t.Target)
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}
