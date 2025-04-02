package h8sproxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
)

type WSConn struct {
	// Conn is the underlying WebSocket connection, write to this
	Conn *websocket.Conn
	// Send is a channel to send messages to the WebSocket
	Send chan []byte
	// The NATS subject the incoming data is published.
	PublishSubject string
	// The NATS _INBOX or subject to receive data on.
	SubscribeSubject string
	// Used to store headers from the HTTP request that sets up the WebSocket connection.
	Headers http.Header
}

type WSPool struct {
	sync.RWMutex
	conns map[string]*WSConn // key = Sec-WebSocket-Key (raw)
}

func NewWSPool() *WSPool {
	return &WSPool{
		conns: make(map[string]*WSConn),
	}
}

func (p *WSPool) Set(secKey string, conn *WSConn) {
	p.Lock()
	defer p.Unlock()
	p.conns[secKey] = conn
}

func (p *WSPool) Get(secKey string) *WSConn {
	p.RLock()
	defer p.RUnlock()
	return p.conns[secKey]
}

func (p *WSPool) Remove(secKey string) {
	p.Lock()
	defer p.Unlock()
	delete(p.conns, secKey)
}

type Option func(*H8Sproxy)

type H8Sproxy struct {
	NATSConn       *nats.Conn
	RequestTimeout time.Duration
	// HostFilters is a list of host filters to apply to incoming requests.
	HostFilters []string
	InboxPrefix string
	WSPool      *WSPool
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// TODO add origin check if necessary
		return true
	},
}

func NewH8Sproxy(natsConn *nats.Conn, opts ...Option) *H8Sproxy {
	proxy := &H8Sproxy{
		NATSConn:       natsConn,
		RequestTimeout: time.Second * 2,
		InboxPrefix:    "_INBOX.h8s.",
		WSPool:         NewWSPool(),
	}
	for _, opt := range opts {
		opt(proxy)
	}
	return proxy
}

func WithHostFilter(filter string) Option {
	return func(h8s *H8Sproxy) {
		h8s.HostFilters = append(h8s.HostFilters, filter)
	}
}

func WithRequestTimeout(timeout time.Duration) Option {
	return func(h8s *H8Sproxy) {
		h8s.RequestTimeout = timeout
	}
}

func WithInboxPrefix(prefix string) Option {
	return func(h8s *H8Sproxy) {
		h8s.InboxPrefix = prefix
	}
}

func (h8s *H8Sproxy) Handler(res http.ResponseWriter, req *http.Request) {
	if strings.EqualFold(req.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		h8s.handleWebSocket(res, req)
		fmt.Println("WebSocket upgrade")
		return
	}

	msg := httpReqToNATS(req)
	reply, err := h8s.NATSConn.RequestMsg(msg, h8s.RequestTimeout)
	if err != nil {
		slog.Error("Failed to publish message", "error", err)
		http.Error(res, fmt.Sprintf("Error: %s", err), http.StatusGatewayTimeout)
		return
	}
	slog.Info("Received reply", "reply-inbox", reply.Sub.Subject)
}

func (h8s *H8Sproxy) Dummy(res http.ResponseWriter, req *http.Request) {
	fmt.Println("Dummy")
}

func (h8s *H8Sproxy) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	secKey := r.Header.Get("Sec-WebSocket-Key")
	if secKey == "" {
		slog.Warn("Missing Sec-WebSocket-Key")
		http.Error(w, "Missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}

	cleanHost := cleanHost(r.Host)
	publishSubject := "h8s.ws." + reverseHost(cleanHost) + "." + pathSegments(r.URL.Path)
	subscribeSubject := nats.NewInbox()

	wsConn := &WSConn{
		Conn:             conn,
		Send:             make(chan []byte, 256),
		PublishSubject:   publishSubject,
		SubscribeSubject: subscribeSubject,
		Headers:          r.Header.Clone(),
	}

	h8s.WSPool.Set(secKey, wsConn)
	defer h8s.WSPool.Remove(secKey)

	// Subscribe to per-client reply subject
	sub, err := h8s.NATSConn.Subscribe(subscribeSubject, func(msg *nats.Msg) {
		select {
		case wsConn.Send <- msg.Data:
		default:
			slog.Warn("Send channel full, dropping message", "subject", msg.Subject)
		}
	})
	if err != nil {
		slog.Error("Failed to subscribe to NATS reply subject", "error", err)
		return
	}
	defer sub.Unsubscribe()

	// Write data over WebSocket
	go func() {
		for msg := range wsConn.Send {
			err := wsConn.Conn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				slog.Warn("WebSocket write failed", "error", err)
				return
			}
		}
	}()

	// Read data from WebSocket
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			slog.Info("WebSocket read closed", "error", err)
			break
		}
		natsMsg := &nats.Msg{
			Subject: wsConn.PublishSubject,
			Reply:   wsConn.SubscribeSubject,
			Data:    msg,
			Header:  nats.Header{},
		}

		// Propagate selected headers from wsConn.Headers to NATS
		for key, values := range wsConn.Headers {
			for _, value := range values {
				natsMsg.Header.Add(key, value)
			}
		}

		err = h8s.NATSConn.PublishMsg(natsMsg)
		if err != nil {
			slog.Error("Failed to publish to NATS", "error", err)
			break
		}
	}
}

func httpReqToNATS(req *http.Request) *nats.Msg {
	cleanHost := cleanHost(req.Host)
	subject := "h8s." + req.Method + "." + reverseHost(cleanHost) + "." + pathSegments(req.URL.Path)
	msg := nats.NewMsg(subject)
	for key, value := range req.Header {
		for _, v := range value {
			msg.Header.Add(key, v)
		}
	}
	return msg
}

func reverseHost(host string) string {
	parts := strings.Split(sanitizeHostForSubject(host), ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

func pathToNATSSubject(path string) string {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		decoded = path // fallback to original if malformed
	}
	segments := strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' })
	safeSegments := make([]string, len(segments))
	for i, s := range segments {
		safeSegments[i] = sanitizeForNATS(s)
	}
	if len(safeSegments) == 0 {
		return "root"
	}
	return strings.Join(safeSegments, ".")
}

func sanitizeForNATS(s string) string {
	// Replace dots to avoid breaking NATS subjects
	s = strings.ReplaceAll(s, ".", "_")
	// Optionally replace other reserved characters here if needed
	return s
}

func cleanHost(rawHost string) string {
	host, _, err := net.SplitHostPort(rawHost)
	if err != nil {
		// If no port, assume rawHost is clean
		host = rawHost
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.String() // e.g., 127.0.0.1
	}
	return host // e.g., "localhost"
}

func sanitizeHostForSubject(host string) string {
	return strings.NewReplacer(
		":", "_", // colons not allowed
	).Replace(host)
}

func pathSegments(path string) string {
	// We remove the first "/"
	return strings.Join(strings.Split(pathToNATSSubject(path[1:]), "/"), ".")
}
