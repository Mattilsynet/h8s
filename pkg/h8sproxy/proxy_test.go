package h8sproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func startEmbeddedNATSServer(t *testing.T) *server.Server {
	opts := &server.Options{
		Port: 4223,
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err)

	go ns.Start()
	if !ns.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS server not ready in time")
	}

	t.Cleanup(func() {
		ns.Shutdown()
	})

	return ns
}

func TestHttpReqToNATS_RequestReply_HeaderPropagation(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	tests := []struct {
		name            string
		method          string
		host            string
		path            string
		headers         http.Header
		expectedReply   []byte
		expectedHeaders map[string]string // key: header name, value: expected value
	}{
		{
			name:          "GET with X-Test header",
			method:        "GET",
			host:          "api.example.io",
			path:          "/ping",
			headers:       http.Header{"X-Test": []string{"true"}, "X-Env": []string{"staging"}},
			expectedReply: []byte("pong"),
			expectedHeaders: map[string]string{
				"X-Test": "true",
				"X-Env":  "staging",
			},
		},
		{
			name:          "POST with Authorization header",
			method:        "POST",
			host:          "service.internal",
			path:          "/submit/data",
			headers:       http.Header{"Authorization": []string{"Bearer abc123"}},
			expectedReply: []byte("ack"),
			expectedHeaders: map[string]string{
				"Authorization": "Bearer abc123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject := "h8s." + tt.method + "." + reverseHost(tt.host) + "." + pathSegments(tt.path)

			// Set up a responder that checks headers and replies
			_, err := nc.Subscribe(subject, func(msg *nats.Msg) {
				for key, expected := range tt.expectedHeaders {
					got := msg.Header.Get(key)
					require.Equal(t, expected, got, "header %q mismatch", key)
				}
				nc.PublishMsg(&nats.Msg{
					Subject: msg.Reply,
					Data:    tt.expectedReply,
				})
			})
			require.NoError(t, err)

			reqURL, _ := url.Parse("http://" + tt.host + tt.path)
			req := &http.Request{
				Method: tt.method,
				Host:   tt.host,
				URL:    reqURL,
				Header: tt.headers,
			}

			msg := httpReqToNATS(req)
			replySubj := nats.NewInbox()
			replySub, err := nc.SubscribeSync(replySubj)
			require.NoError(t, err)
			msg.Reply = replySubj

			err = nc.PublishMsg(msg)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			replyMsg, err := replySub.NextMsgWithContext(ctx)
			require.NoError(t, err)
			require.Equal(t, tt.expectedReply, replyMsg.Data)
		})
	}
}

// TestHandleWebSocket_NATSRequestReply tests the WebSocket handler with NATS request-reply
// Apologise for the horrid code
func TestHandleWebSocket_NATSRequestReply(t *testing.T) {
	// Start embedded NATS server
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Create proxy
	proxy := &H8Sproxy{
		NATSConn:       nc,
		RequestTimeout: 5 * time.Second,
		WSPool:         NewWSPool(),
	}

	// Setup test server
	srv := httptest.NewServer(http.HandlerFunc(proxy.handleWebSocket))
	t.Cleanup(srv.Close)

	// NATS subject key based on host+path
	subject := "h8s.ws.localhost.test.foo"
	t.Logf("Publishing on NATS subject: %s", subject)

	// Subscribe on backend NATS side to reply, simulating a backend server
	_, err = nc.Subscribe(subject, func(msg *nats.Msg) {
		t.Logf("NATS subscriber got: %s", string(msg.Data))
		t.Logf("Headers on NATS message: %v", msg.Header)
		_ = nc.Publish(msg.Reply, []byte("pong"))
		t.Logf("Reply on NATS subject: %s", msg.Reply)
	})
	require.NoError(t, err)

	// Dial WebSocket
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/test/foo"
	u, _ := url.Parse(wsURL)

	ws, _, err := websocket.DefaultDialer.Dial(
		u.String(),
		http.Header{"Host": []string{"localhost"}})
	require.NoError(t, err)
	defer ws.Close()

	// Send a message to trigger the handler
	err = ws.WriteMessage(websocket.TextMessage, []byte("ping"))
	require.NoError(t, err)

	// Expect the response from NATS over WS
	_, resp, err := ws.ReadMessage()
	fmt.Println(string(resp))
	require.NoError(t, err)
	require.Equal(t, "pong", string(resp))
}
