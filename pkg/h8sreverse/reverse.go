package h8sreverse

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
)

// ReverseProxy forwards NATS requests to HTTP and publishes back.
// It can subscribe to a filtered set of subjects based on the subjectmapper rules.

type ReverseProxy struct {
	nats    *nats.Conn
	client  *http.Client
	wsConns sync.Map // map[string]*websocket.Conn (key is Reply subject)
}

// NewReverseProxy creates a proxy with a default HTTP client.
func NewReverseProxy(nc *nats.Conn) *ReverseProxy {
	return &ReverseProxy{
		nats:   nc,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// SubscribeOptions holds optional filter settings.
// If Methods is empty, all HTTP methods are accepted.
// If Paths is empty, all paths are accepted.
// If Host is empty, all hosts are accepted.
// Subscriptions are constructed using subjectmapper.SubjectPrefix and wildcards.
// Example: Methods: []string{"GET", "POST"} => subject "h8s.http.GET.*".
// Paths: []string{"/api/users"} => subject "h8s.http.GET.h8s/..api/users".
// This function builds the wildcard subjects accordingly.
func (r *ReverseProxy) SubscribeAll(ctx context.Context) error {
	patterns := []string{
		subjectmapper.SubjectPrefix + ".http.*.*.>",
		subjectmapper.SubjectPrefix + ".http.*.*", // Handle root path (no path segments)
		subjectmapper.SubjectPrefix + ".ws.ws.*.>",
		"h8s.control.ws.conn.established",
		"h8s.control.ws.conn.closed",
	}
	for _, pat := range patterns {
		_, err := r.nats.Subscribe(pat, r.handleMsg)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ReverseProxy) handleMsg(msg *nats.Msg) {
	if msg.Subject == "h8s.control.ws.conn.established" {
		r.handleControlEstablished(msg)
		return
	}
	if msg.Subject == "h8s.control.ws.conn.closed" {
		r.handleControlClosed(msg)
		return
	}

	parts := strings.Split(msg.Subject, ".")

	if len(parts) > 3 && parts[1] == "ws" && parts[2] == "ws" {
		// WebSocket data frame
		r.handleWebSocketFrame(msg)
		return
	}

	// parts < 4 is invalid, but parts == 4 means root path (path is empty)
	if len(parts) < 4 {
		slog.Error("invalid subject", "subject", msg.Subject)
		return
	}

	scheme := parts[1]
	method := parts[2]
	host := parts[3]

	var path string
	if len(parts) == 5 {
		path = parts[4]
	} else if len(parts) > 5 {
		path = strings.Join(parts[4:], "/")
	}

	urlStr := scheme + "://" + host + "/" + path
	req, err := http.NewRequest(method, urlStr, bytes.NewReader(msg.Data))
	if err != nil {
		slog.Error("new request", "error", err)
		return
	}
	for k, v := range msg.Header {
		for _, vv := range v {
			req.Header.Add(k, vv)
		}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		slog.Error("http error", "error", err)
		r.publishError(msg, 502, err.Error())
		return
	}
	defer resp.Body.Close()

	// Streaming logic:
	// 1. Strip Content-Length to force h8sproxy into streaming mode.
	// 2. Read from response body in chunks.
	// 3. Publish first chunk with headers.
	// 4. Publish subsequent chunks.
	// 5. Publish empty message to signal EOF.

	header := nats.Header{}
	for k, v := range resp.Header {
		for _, vv := range v {
			header.Add(k, vv)
		}
	}
	header.Set("Status", http.StatusText(resp.StatusCode))
	header.Set("Status-Code", strconv.Itoa(resp.StatusCode))
	// Remove Content-Length to ensure h8sproxy streams the response
	header.Del("Content-Length")

	buf := make([]byte, 4*1024)
	first := true

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			msgData := make([]byte, n)
			copy(msgData, buf[:n])

			respMsg := &nats.Msg{
				Subject: msg.Reply,
				Data:    msgData,
			}
			if first {
				respMsg.Header = header
				first = false
			}
			r.nats.PublishMsg(respMsg)
		}

		if err != nil {
			if err == io.EOF {
				// Send empty message to close the stream on h8sproxy side
				// If first is still true (empty body), we must attach headers here
				finishMsg := &nats.Msg{
					Subject: msg.Reply,
					Data:    []byte{},
				}
				if first {
					finishMsg.Header = header
				}
				r.nats.PublishMsg(finishMsg)
			} else {
				slog.Error("read body error", "error", err)
				// Best effort close
				r.nats.PublishMsg(&nats.Msg{Subject: msg.Reply, Data: []byte{}})
			}
			break
		}
	}
}

func (r *ReverseProxy) publishError(msg *nats.Msg, code int, errStr string) {
	respMsg := &nats.Msg{
		Subject: msg.Reply,
		Data:    []byte(errStr),
		Header:  nats.Header{},
	}
	respMsg.Header.Set("Status-Code", strconv.Itoa(code))
	respMsg.Header.Set("Status", http.StatusText(code))
	r.nats.PublishMsg(respMsg)
}

func (r *ReverseProxy) handleControlEstablished(msg *nats.Msg) {
	// Use Host header to determine backend URL
	host := msg.Header.Get("Host")
	if host == "" {
		// Fallback to subject parsing if Host header is missing (less reliable)
		publishSubject := msg.Header.Get("X-H8s-PublishSubject")
		if publishSubject == "" {
			slog.Warn("handleControlEstablished: missing headers")
			return
		}
		parts := strings.Split(publishSubject, ".")
		if len(parts) >= 4 {
			host = parts[3]
		}
	}

	// Construct URL. Path?
	// We might need "X-H8s-Original-Path" or similar if Subject doesn't have it.
	// Subject has path at the end.

	// Let's use the X-H8s-PublishSubject for path extraction only if needed,
	// but actually, can we just assume /? No.
	// H8SOriginalQueryHTTPHeaderName is in h8sproxy.
	// What about Path?
	// h8sproxy `httpRequestToNATSMessage` adds `X-H8s-PublishSubject`.
	// And `req.URL.Path` is embedded in the subject.

	// Let's stick to extraction from subject for path, but Host from header.
	publishSubject := msg.Header.Get("X-H8s-PublishSubject")
	path := ""
	if publishSubject != "" {
		parts := strings.Split(publishSubject, ".")
		if len(parts) > 4 {
			path = "/" + strings.Join(parts[4:], "/")
		}
	}

	u := "ws://" + host + path

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	// Forward headers using Add/Set but filtering specific connection headers
	headers := http.Header{}
	for k, v := range msg.Header {
		if strings.HasPrefix(k, "X-H8s-") {
			continue
		}
		// Skip headers managed by Dial
		if k == "Sec-Websocket-Key" || k == "Connection" || k == "Upgrade" || strings.HasPrefix(k, "Sec-Websocket-") {
			continue
		}
		// Case insensitive check for canonical keys if needed, but NATS headers match HTTP usually
		if strings.EqualFold(k, "Sec-WebSocket-Key") || strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Upgrade") || strings.HasPrefix(strings.ToLower(k), "sec-websocket-") {
			continue
		}
		for _, vv := range v {
			headers.Add(k, vv)
		}
	}

	wsConn, _, err := dialer.Dial(u, headers)
	if err != nil {
		slog.Error("handleControlEstablished: failed to dial backend", "url", u, "error", err)
		return
	}

	// Store connection mapping: ReplySubject -> *websocket.Conn
	r.wsConns.Store(msg.Reply, wsConn)
	slog.Info("handleControlEstablished: connected", "url", u, "reply", msg.Reply)

	// Pump messages from backend -> NATS
	go func() {
		defer func() {
			wsConn.Close()
			r.wsConns.Delete(msg.Reply)
		}()
		for {
			_, message, err := wsConn.ReadMessage()
			if err != nil {
				slog.Info("backend ws read error", "err", err)
				break
			}
			r.nats.Publish(msg.Reply, message)
		}
	}()
}

func (r *ReverseProxy) handleControlClosed(msg *nats.Msg) {
	slog.Info("handleControlClosed", "reply", msg.Reply)
	if val, ok := r.wsConns.Load(msg.Reply); ok {
		if conn, ok := val.(*websocket.Conn); ok {
			conn.Close()
		}
		r.wsConns.Delete(msg.Reply)
	}
}

func (r *ReverseProxy) handleWebSocketFrame(msg *nats.Msg) {
	val, ok := r.wsConns.Load(msg.Reply)
	if !ok {
		// This can happen if the control message hasn't arrived or was missed
		// Or if the connection was already closed.
		// For now we just drop it.
		// slog.Warn("handleWebSocketFrame: connection not found", "reply", msg.Reply)
		return
	}
	conn := val.(*websocket.Conn)
	err := conn.WriteMessage(websocket.TextMessage, msg.Data)
	if err != nil {
		slog.Error("handleWebSocketFrame: write error", "error", err)
		conn.Close()
		r.wsConns.Delete(msg.Reply)
	}
}
