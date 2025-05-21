package h8sproxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

const (
	H8SControlSubjectPrefix = "h8s.control"
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

func (p *WSPool) ActiveConnections() int {
	p.RLock()
	defer p.RUnlock()
	return len(p.conns)
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

func (h8s *H8Sproxy) Handler(res http.ResponseWriter, req *http.Request) {
	if strings.EqualFold(req.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		h8s.handleWebSocket(res, req)
		fmt.Println("WebSocket upgrade")
		return
	}
	// Scheme is not set in request, which is strange, we'll enforce that here.
	req.URL.Scheme = "http"
	msg := httpRequestToNATSMessage(req)

	reply, err := h8s.NATSConn.RequestMsg(msg, h8s.RequestTimeout)
	if err != nil {
		slog.Error("Failed to publish message", "error", err)
		http.Error(res, fmt.Sprintf("Error: %s", err), http.StatusGatewayTimeout)
		return
	}
	slog.Debug("Received reply", "reply-inbox", reply.Sub.Subject)

	// write reply headers to responsewriter
	for key, values := range reply.Header {
		res.Header().Add(key, strings.Join(values, ","))
	}
	res.Header().Add("Content-Length", fmt.Sprintf("%d", len(reply.Data)))
	// write reply.Data to responsewriter
	res.Write(reply.Data)
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

	fmt.Println("WebSocket upgrade successful", r.Proto)
	secKey := r.Header.Get("Sec-WebSocket-Key")
	if secKey == "" {
		slog.Warn("Missing Sec-WebSocket-Key")
		http.Error(w, "Missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}

	sm := subjectmapper.NewSubjectMap(r)
	subscribeSubject := sm.InboxSubjectPrefix()

	wsConn := &WSConn{
		Conn:             conn,
		Send:             make(chan []byte, 256),
		PublishSubject:   sm.WebSocketPublishSubject(),
		SubscribeSubject: subscribeSubject,
		Headers:          r.Header.Clone(),
	}

	// If the connection is not in the pool, do a handshake publish with 0 bytes.
	if h8s.WSPool.Get(secKey) == nil {
		h8s.cmConnectionEstablished(wsConn)
	}

	h8s.WSPool.Set(secKey, wsConn)
	defer h8s.WSPool.Remove(secKey)

	// Subscribe to per-client reply subject, get data from the backend, and send to client.
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

	// Write data over WebSocket, send reply(message payload) to client.
	go func() {
		for msg := range wsConn.Send {
			err := wsConn.Conn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				slog.Warn("WebSocket write failed", "error", err)
				return
			}
		}
	}()

	// Read data from WebSocket connection and publish to nats.
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

		fmt.Println("Publishing to NATS subject:", natsMsg)
		err = h8s.NATSConn.PublishMsg(natsMsg)
		if err != nil {
			slog.Error("Failed to publish to NATS", "error", err)
			break
		}
	}
}

// cmConnectionEstablished publishes a message on the control channel indicating that
// a websocket connection has been established.
func (h8s *H8Sproxy) cmConnectionEstablished(wsConn *WSConn) {
	// If the connection is not in the pool, do a handshake publish with 0 bytes.
	handshakeMsg := &nats.Msg{
		Subject: wsConn.PublishSubject,
		Reply:   wsConn.SubscribeSubject,
		Header:  nats.Header(wsConn.Headers),
	}

	// Soft include of controlMessage on establish of websocket connection.
	controlMessage := &nats.Msg{
		Subject: H8SControlSubjectPrefix + ".ws.conn.established",
		Reply:   wsConn.SubscribeSubject,
		Header:  nats.Header(wsConn.Headers),
	}
	controlMessage.Header.Add("X-H8S-PublishSubject", wsConn.PublishSubject)

	err := h8s.NATSConn.PublishMsg(handshakeMsg)
	if err != nil {
		slog.Error("failed to publish to NATS", "error", err)
	}
	slog.Info("published handshake message", "subject", controlMessage.Subject)

	controlErr := h8s.NATSConn.PublishMsg(controlMessage)
	if controlErr != nil {
		slog.Error(
			"unable to publish control message",
			"error", controlErr)
	}
	slog.Info(
		"published control message",
		"subject", wsConn.PublishSubject)
}

func (h8s *H8Sproxy) cmConnectionClosed(secKey string) {
	wsConn := h8s.WSPool.Get(secKey)
	if wsConn == nil {
		return
	}
}

func httpRequestToNATSMessage(req *http.Request) *nats.Msg {
	sm := subjectmapper.NewSubjectMap(req)
	msg := nats.NewMsg(sm.PublishSubject())
	msg.Reply = fmt.Sprintf("%v.%v", sm.InboxSubjectPrefix(), nuid.Next())
	// Put all headers in the NATS message
	for key, value := range req.Header {
		for _, v := range value {
			msg.Header.Add(key, v)
		}
	}

	// Add propagation of nats subject as X-H8S-Subject header
	// This can be used to inform downstream business logic that
	// does not do any direct NATS communication.
	msg.Header.Add("X-H8S-Subject", msg.Subject)

	return msg
}
