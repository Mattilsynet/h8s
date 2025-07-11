package h8sproxy

import (
	"context"
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
		scheme          string
		headers         http.Header
		expectedReply   []byte
		expectedHeaders map[string]string // key: header name, value: expected value
	}{
		{
			name:          "GET with X-Test header",
			method:        "GET",
			host:          "api.example.io",
			scheme:        "http",
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
			scheme:        "http",
			path:          "/submit/data/test",
			headers:       http.Header{"Authorization": []string{"Bearer abc123"}},
			expectedReply: []byte("pong"),
			expectedHeaders: map[string]string{
				"Authorization": "Bearer abc123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rm := subjectmapper.NewSubjectMap(&http.Request{
				Host:   tt.host,
				Header: tt.headers,
				Method: tt.method,
				URL: &url.URL{
					Scheme: tt.scheme,
					Path:   tt.path,
				},
			})

			subject := rm.PublishSubject()
			t.Log("Subscribing on NATS subject:", subject)
			// subject := "h8s." + tt.method + "." + urltosub.ReverseHost(tt.host) + "." + urltosub.PathSegments(tt.path)

			// Set up a responder(subscriber) that checks headers and replies
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

			// Build the synthetic request
			reqURL, _ := url.Parse("http://" + tt.host + tt.path)
			req := &http.Request{
				Method: tt.method,
				Host:   tt.host,
				URL:    reqURL,
				Header: tt.headers,
				Body:   http.NoBody,
			}
			msg := httpRequestToNATSMessage(req)

			replySub, err := nc.SubscribeSync(msg.Reply)
			require.NoError(t, err)
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

// TestHandleWebSocket_NATSRequestReply tests the WebSocket handler with NATS "request-reply"
// Apologise for the horrid code
func TestHandleWebSocket_NATSPubSubAndWS(t *testing.T) {
	// Start embedded NATS server
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())

	// nc, err := nats.Connect("nats://localhost:4222")
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	proxy := NewH8Sproxy(nc, WithRequestTimeout(5*time.Second))

	// Setup test server
	srv := httptest.NewServer(http.HandlerFunc(proxy.handleWebSocket))
	t.Cleanup(srv.Close)

	// Create a test WebSocket URL and http.Request dummy to inform SubjectMapper
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/test/foo"
	sm := subjectmapper.NewWebSocketMap(wsURL)

	// Subscribe on backend NATS side to reply, simulating a backend server
	t.Log("Subscribing on NATS subject for replies:", sm.WebSocketPublishSubject())
	_, subErr := nc.Subscribe(sm.WebSocketPublishSubject(), func(msg *nats.Msg) {
		reply := &nats.Msg{
			Subject: msg.Reply,
			Data:    []byte("pong"),
		}
		err = nc.PublishMsg(reply)
		if err != nil {
			t.Logf("Error publishing reply: %v", err)
		}
	})
	require.NoError(t, subErr)

	// Dial WebSocket, creates the connection, and populates the wsconn pool
	u, _ := url.Parse(wsURL)
	ws, _, err := websocket.DefaultDialer.Dial(
		u.String(),
		http.Header{"X-WS-Test": []string{"Websocket yay!"}})
	require.NoError(t, err)
	defer ws.Close()

	// Send message to trigger the handler
	err = ws.WriteMessage(websocket.TextMessage, []byte("ping"))
	require.NoError(t, err)

	//	time.Sleep(200 * time.Millisecond) // Give time for the message to be processed

	// Expect the response from NATS over WS
	_, resp, err := ws.ReadMessage()
	t.Log("Response", string(resp))
	require.NoError(t, err)
	require.Equal(t, "pong", string(resp))

	t.Log("Draining NATS connection...")
	nc.Drain()
	t.Log("Waiting 2 seconds before shutting down...")
	time.Sleep(2 * time.Second) // Give time for the message to be processed
}
