package h8sproxy

import (
	"bufio"
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
		Port: -1, // Dynamic port allocation to avoid conflicts
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err)

	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
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

	// Send message to trigger the handler
	err = ws.WriteMessage(websocket.TextMessage, []byte("ping"))
	require.NoError(t, err)

	//	time.Sleep(200 * time.Millisecond) // Give time for the message to be processed

	// Expect the response from NATS over WS
	_, resp, err := ws.ReadMessage()
	t.Log("Response", string(resp))
	require.NoError(t, err)
	require.Equal(t, "pong", string(resp))

	// Explicitly close the WS connection
	require.NoError(t, ws.Close())

	t.Log("Draining NATS connection...")
	nc.Drain()
	t.Log("Waiting 2 seconds before shutting down...")
	time.Sleep(2 * time.Second) // Give time for the message to be processed
}

func TestHTTPProxy_ChunkedTransferEncoding(t *testing.T) {
	t.Helper()

	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	// Start the proxy under test
	proxy := NewH8Sproxy(nc, WithRequestTimeout(5*time.Second))

	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.Handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Request we’ll send to the proxy
	method := http.MethodGet
	host := "localhost"
	path := "/chunked/response"

	// Subject mapping used by the proxy
	rm := subjectmapper.NewSubjectMap(&http.Request{
		Method: method,
		Host:   host,
		URL:    &url.URL{Scheme: "http", Path: path},
	})
	pubSubj := rm.PublishSubject()
	t.Logf("Backend subscribing to subject: %s", pubSubj)

	chunks := [][]byte{
		[]byte("part-0\n"),
		[]byte("part-1\n"),
		[]byte("part-2\n"),
		[]byte("part-3\n"),
	}

	// Backend: publish multiple replies to the dynamic _INBOX reply
	sub, err := nc.Subscribe(pubSubj, func(m *nats.Msg) {
		if m.Reply == "" {
			t.Errorf("expected reply subject (_INBOX.*), got empty")
			return
		}
		for _, c := range chunks {
			if pubErr := nc.PublishMsg(&nats.Msg{Subject: m.Reply, Data: c}); pubErr != nil {
				t.Logf("publish chunk error: %v", pubErr)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Ensure the subscription is registered before sending the HTTP request
	require.NoError(t, nc.FlushTimeout(1*time.Second))

	// Send the HTTP request to the proxy
	req, err := http.NewRequest(method, srv.URL+path, nil)
	require.NoError(t, err)
	req.Host = host

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// For HTTP/1.x, chunked is expected (no Content-Length)
	if resp.ProtoMajor == 1 {
		require.Contains(t, resp.TransferEncoding, "chunked")
		require.Empty(t, resp.Header.Get("Content-Length"))
	}

	// Read progressively and stop once we've seen all expected chunks
	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
		if len(got) == len(chunks) {
			break
		}
	}
	require.NoError(t, sc.Err())

	want := []string{"part-0", "part-1", "part-2", "part-3"}
	require.Equal(t, want, got, "proxy should stream each NATS reply to the HTTP client in order")
}
