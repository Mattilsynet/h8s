// Package h8sproxy provides a reverse proxy for HTTP and WebSocket connections.
// Can be used as a http handler.
package h8sproxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	H8SControlSubjectPrefix                 = "h8s.control"
	H8SControlWebsocketAll                  = H8SControlSubjectPrefix + ".ws.conn.*"
	H8SControlWebsocketEstablished          = H8SControlSubjectPrefix + ".ws.conn.established"
	H8SControlWebsocketClosed               = H8SControlSubjectPrefix + ".ws.conn.closed"
	H8SInterestControlSubject               = H8SControlSubjectPrefix + ".interest"
	H8SControlConnectionClosedSubjectPrefix = H8SControlSubjectPrefix + ".connection.closed"

	H8SPublishSubjectHTTPHeaderName     = "X-H8s-PublishSubject"
	H8SOriginalQueryHTTPHeaderName      = "X-H8s-Original-Query"
	H8SReplySubjectHTTPHeaderName       = "X-H8s-ReplySubject"
	H8SConnectionCloseSubjectHeaderName = "X-H8s-Connection-Close-Subject"
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

type H8SInterestTracker interface {
	Run() error
	ValidRequest(req http.Request) bool
}

type H8Sproxy struct {
	NATSConn       *nats.Conn
	RequestTimeout time.Duration
	// HostFilters is a list of allowed hostnames.
	HostFilters []string
	// InterestOnly flag indicates if H8Sproxy should only serve
	// traffic for registered interest. Downstream clients communicate
	// interest over NATS on the h8s.control.interest subject.
	InterestOnly    bool
	InterestTracker H8SInterestTracker
	WSPool          *WSPool
	MaxBodySize     int64

	// With this set to true, h8s will not expect a reply. All incoming requests will be publish only.
	PublishOnly bool
	// NaiveAuthorizationKey when set will go a "naive" authorization against key on all endpoints.
	NaiveAuthorizationKey string
	// AllowedOrigins configures optional WebSocket origin allowlist.
	AllowedOrigins []string

	OTELTracer trace.Tracer // OpenTelemetry tracer for this connection
	OTELMeter  metric.Meter // OpenTelemetry meter for this connection

	// ProxyID is a unique identifier for this proxy instance, used for routing replies.
	ProxyID string
	// upgrader handles WebSocket upgrade requests.
	upgrader websocket.Upgrader
	// pendingReqs tracks active requests waiting for responses.
	// Key is the request ID (last part of the reply subject), value is a generic channel for NATS messages.
	// We use sync.Map for concurrent access.
	pendingReqs sync.Map

	NumberOfRequests             metric.Int64Counter // Number of requests handled by this proxy
	NumberOfDeniedRequests       metric.Int64Counter //
	NumberOfFailedRequests       metric.Int64Counter // Number of failed requests
	NumberOfWebsocketConnections metric.Int64Gauge   // Number of WebSocket connections established
	NumberOfInterests            metric.Int64Gauge   // Number of interests registered
}

func NewH8Sproxy(natsConn *nats.Conn, opts ...Option) *H8Sproxy {
	proxy := &H8Sproxy{
		NATSConn:       natsConn,
		RequestTimeout: time.Second * 30,
		WSPool:         NewWSPool(),
		InterestOnly:   false,
		PublishOnly:    false,
		MaxBodySize:    2 * 1024 * 1024, // Default 2MB
		// The OTEL Meter and Tracer by default get a NOOP by default.
		OTELTracer: otel.GetTracerProvider().Tracer("h8s-proxy"),
		OTELMeter:  otel.GetMeterProvider().Meter("h8s-proxy"),
		ProxyID:    nuid.Next(),
	}
	for _, opt := range opts {
		opt(proxy)
	}

	// SECURITY NOTE: CheckOrigin is permissive unless AllowedOrigins is set.
	// Origin validation can be enforced by configuring AllowedOrigins.
	proxy.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			if len(proxy.AllowedOrigins) == 0 {
				return true
			}
			origin := r.Header.Get("Origin")
			for _, allowed := range proxy.AllowedOrigins {
				if strings.EqualFold(origin, allowed) {
					return true
				}
			}
			return false
		},
	}

	if proxy.InterestOnly {
		slog.Info("Starting with interest mode only")
		if err := proxy.InterestTracker.Run(); err != nil {
			slog.Error("failed to start InterestTracker", "error", err)
			os.Exit(1)
		}
	}

	var err error
	proxy.NumberOfRequests, err = proxy.OTELMeter.Int64Counter(
		"h8s_number_of_requests",
		metric.WithDescription("Counts all requests received by h8s. Both HTTP and Websocket."))
	if err != nil {
		slog.Error("failed to create NumberOfRequests metric", "error", err)
	}

	proxy.NumberOfDeniedRequests, err = proxy.OTELMeter.Int64Counter(
		"h8s_number_of_denied_requests",
		metric.WithDescription("Counts the number of denied requests either by hots filter or interest filter."))
	if err != nil {
		slog.Error("failed to create NumberOfRequests metric", "error", err)
	}

	proxy.NumberOfFailedRequests, err = proxy.OTELMeter.Int64Counter(
		"h8s_number_of_failed_requests",
		metric.WithDescription("Counts the requests that failed without a specific reason."))
	if err != nil {
		slog.Error("failed to create NumberOfFailedRequests metric", "error", err)
	}

	proxy.NumberOfWebsocketConnections, err = proxy.OTELMeter.Int64Gauge(
		"h8s_active_websocket_connections",
		metric.WithDescription("Counts the requests that does a websocket upgrade and becomes a websocket connection."))
	if err != nil {
		slog.Error("failed to create NumberOfWebsocketConnections metric", "error", err)
	}

	proxy.NumberOfDeniedRequests, err = proxy.OTELMeter.Int64Counter(
		"h8s_number_of_denied_requests",
		metric.WithDescription("Counts the number of denied requests either by hots filter or interest filter."))
	if err != nil {
		slog.Error("failed to create NumberOfRequests metric", "error", err)
	}

	proxy.NumberOfFailedRequests, err = proxy.OTELMeter.Int64Counter(
		"h8s_number_of_failed_requests",
		metric.WithDescription("Counts the requests that failed without a specific reason."))
	if err != nil {
		slog.Error("failed to create NumberOfFailedRequests metric", "error", err)
	}

	proxy.NumberOfWebsocketConnections, err = proxy.OTELMeter.Int64Gauge(
		"h8s_active_websocket_connections",
		metric.WithDescription("Counts the requests that does a websocket upgrade and becomes a websocket connection."))
	if err != nil {
		slog.Error("failed to create NumberOfWebsocketConnections metric", "error", err)
	}
	// Subscribe to persistent reply subject for this proxy instance
	replySubject := fmt.Sprintf("%s.%s.*", subjectmapper.InboxPrefix, proxy.ProxyID)
	_, err = proxy.NATSConn.Subscribe(replySubject, proxy.dispatch)
	if err != nil {
		slog.Error("failed to subscribe to reply subject", "subject", replySubject, "error", err)
		os.Exit(1)
	}
	slog.Info("Subscribed to reply subject", "subject", replySubject)

	return proxy
}

// dispatch handles incoming NATS messages and routes them to the correct request channel.
func (h8s *H8Sproxy) dispatch(msg *nats.Msg) {
	// Subject format: _INBOX.h8s.<ProxyID>.<RequestID>
	parts := strings.Split(msg.Subject, ".")
	if len(parts) < 4 {
		return
	}
	reqID := parts[len(parts)-1]

	if ch, ok := h8s.pendingReqs.Load(reqID); ok {
		if respChan, ok := ch.(chan *nats.Msg); ok {
			select {
			case respChan <- msg:
			default:
				slog.Warn("response channel full, dropping message", "reqID", reqID)
			}
		}
	}
}

func WithInterestOnly() Option {
	return func(h8s *H8Sproxy) {
		h8s.InterestOnly = true
	}
}

func WithInterestTracker(tracker H8SInterestTracker) Option {
	return func(h8s *H8Sproxy) {
		h8s.InterestTracker = tracker
	}
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

func WithOTELTracer(tracer trace.Tracer) Option {
	return func(h8s *H8Sproxy) {
		h8s.OTELTracer = tracer
	}
}

func WithOTELMeter(meter metric.Meter) Option {
	return func(h8s *H8Sproxy) {
		h8s.OTELMeter = meter
	}
}

func WithNaiveAuthorizationKey(key string) Option {
	return func(h8s *H8Sproxy) {
		h8s.NaiveAuthorizationKey = key
	}
}

func WithPublishOnly() Option {
	return func(h8s *H8Sproxy) {
		h8s.PublishOnly = true
	}
}

func WithMaxBodySize(size int64) Option {
	return func(h8s *H8Sproxy) {
		h8s.MaxBodySize = size
	}
}

func WithAllowedOrigins(origins ...string) Option {
	return func(h8s *H8Sproxy) {
		h8s.AllowedOrigins = append(h8s.AllowedOrigins, origins...)
	}
}

func (h8s *H8Sproxy) Handler(res http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	if h8s.NaiveAuthorizationKey != "" {
		if req.Header.Get("Authorization") != h8s.NaiveAuthorizationKey {
			res.Header().Set("Content-Type", "text/plain")
			res.WriteHeader(http.StatusForbidden)
			return
		}
	}
	h8s.NumberOfRequests.Add(req.Context(), 1,
		metric.WithAttributes(
			attribute.String("method", req.Method),
			attribute.String("path", req.URL.Path),
		))

	// Check HostFilters
	if len(h8s.HostFilters) > 0 {
		hostMatch := false
		for _, filter := range h8s.HostFilters {
			if strings.EqualFold(req.Host, filter) {
				hostMatch = true
				break
			}
			// Check X-Forwarded-Host if present
			if forwardedHost := req.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
				if strings.EqualFold(forwardedHost, filter) {
					hostMatch = true
					break
				}
			}
		}

		if !hostMatch {
			h8s.NumberOfDeniedRequests.Add(req.Context(), 1, metric.WithAttributes(attribute.String("reason", "host_filter")))
			res.Header().Set("Content-Type", "text/plain")
			res.WriteHeader(http.StatusForbidden)
			return
		}
	}

	if h8s.InterestOnly {
		if !h8s.InterestTracker.ValidRequest(*req) {
			h8s.NumberOfDeniedRequests.Add(req.Context(), 1, metric.WithAttributes(attribute.String("reason", "interest_filter")))
			res.Header().Set("Content-Type", "text/plain")
			res.WriteHeader(http.StatusNotFound)
			return
		}
	}

	if strings.EqualFold(req.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		h8s.handleWebSocket(res, req)
		return
	}

	_, span := h8s.OTELTracer.Start(req.Context(), "request")
	defer span.End()

	// Scheme is not set in request, which is strange, we'll enforce that here.
	req.URL.Scheme = "http"

	// Enforce MaxBytesReader to prevent DoS via large bodies
	req.Body = http.MaxBytesReader(res, req.Body, h8s.MaxBodySize)

	msg := httpRequestToNATSMessage(req)

	var (
		wroteHeaders = false
		reason       = "completed"
		once         sync.Once
		subject      = msg.Header.Get(H8SConnectionCloseSubjectHeaderName)
		reqID        = nuid.Next()
	)

	// Set the reply subject to target this specific request ID under the proxy's wildcard subscription
	msg.Reply = fmt.Sprintf("%v.%v.%v", subjectmapper.InboxPrefix, h8s.ProxyID, reqID)

	defer once.Do(func() {
		if subject != "" {
			_ = h8s.NATSConn.Publish(subject, []byte(reason))
			_ = h8s.NATSConn.FlushTimeout(60 * time.Millisecond)
		}
	})

	if h8s.PublishOnly {
		if err := h8s.NATSConn.PublishMsg(msg); err != nil {
			slog.Error("Unable to publish nats msg in PublishOnly mode", "error", "err", "message", msg)
		}
		res.WriteHeader(http.StatusOK)
		return
	}

	// Create and register the response channel
	respChan := make(chan *nats.Msg, 16) // Buffer to handle bursts
	h8s.pendingReqs.Store(reqID, respChan)
	defer h8s.pendingReqs.Delete(reqID)

	// Publish the request
	if err := h8s.NATSConn.PublishMsg(msg); err != nil {
		slog.Error("Failed to publish request", "error", err)
		http.Error(res, "Bad gateway", http.StatusBadGateway)
		return
	}

	// Use a timer for sliding timeout (reset on data)
	timer := time.NewTimer(h8s.RequestTimeout)
	defer timer.Stop()

	flusher, _ := res.(http.Flusher)

loop:
	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			slog.Info("http request context canceled", "err", ctx.Err())
			reason = "ctx_done"
			return

		case <-timer.C:
			// Sliding timeout exceeded
			slog.Info("upstream request timed out (sliding)", "timeout", h8s.RequestTimeout)
			reason = "timeout"
			if !wroteHeaders {
				http.Error(res, "Gateway Timeout", http.StatusGatewayTimeout)
			}
			return

		case rm := <-respChan:
			// Reset sliding timer on every message (even empty ones if they are keepalives)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(h8s.RequestTimeout)

			if !wroteHeaders {
				copyHeadersOnce(res, rm.Header)
				wroteHeaders = true
			}

			// Responses with header Content-Length set are treated as single responses.
			if cl := rm.Header.Get("Content-Length"); cl != "" {
				res.Write(rm.Data)
				return
			}

			// Handle responses without Content-Length (SSE/Chunked transfer encoding)
			if len(rm.Data) > 0 {
				if bytes.Equal(rm.Data, []byte("0\r\n\r\n")) {
					slog.Debug("got terminating chunk")
					reason = "termination_chunk"
					break loop
				}

				if _, werr := res.Write(rm.Data); werr != nil {
					slog.Error("Failed to write response", "error", werr)
					reason = "write_error"
					break loop
				}

				flusher.Flush()
			} else {
				// Empty message usually signals EOF in this protocol
				break loop
			}
		}
	}
}

func (h8s *H8Sproxy) Dummy(res http.ResponseWriter, req *http.Request) {
	fmt.Println("Dummy")
}

func (h8s *H8Sproxy) handleWebSocket(res http.ResponseWriter, req *http.Request) {
	conn, err := h8s.upgrader.Upgrade(res, req, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	if h8s.MaxBodySize > 0 {
		conn.SetReadLimit(h8s.MaxBodySize)
	}

	_, span := h8s.OTELTracer.Start(req.Context(), "websocket-connection")
	defer span.End()

	secKey := req.Header.Get("Sec-WebSocket-Key")
	if secKey == "" {
		slog.Warn("Missing Sec-WebSocket-Key")
		http.Error(res, "Missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}

	sm := subjectmapper.NewSubjectMap(req)
	subscribeSubject := sm.InboxSubjectPrefix()

	wsConn := &WSConn{
		Conn:             conn,
		Send:             make(chan []byte, 256),
		PublishSubject:   sm.WebSocketPublishSubject(),
		SubscribeSubject: subscribeSubject,
		Headers:          req.Header.Clone(),
	}
	defer close(wsConn.Send)

	// If the connection is not in the pool, do a handshake publish with 0 bytes.
	if h8s.WSPool.Get(secKey) == nil {
		h8s.cmConnectionEstablished(wsConn)
	}

	h8s.WSPool.Set(secKey, wsConn)
	defer h8s.WSPool.Remove(secKey)

	h8s.NumberOfWebsocketConnections.Record(req.Context(), int64(h8s.WSPool.ActiveConnections()))

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
				h8s.cmConnectionClosed(wsConn.Headers.Get("Sec-WebSocket-Key"))
				_ = wsConn.Conn.Close()
				return
			}
		}
	}()

	// Read incoming data from client on WebSocket connection and publish to nats.
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			slog.Info("WebSocket read closed", "error", err)
			h8s.cmConnectionClosed(wsConn.Headers.Get("Sec-WebSocket-Key"))
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

// cmConnectionEstablished publishes a message on the control channel indicating that
// a websocket connection has been established.
func (h8s *H8Sproxy) cmConnectionEstablished(wsConn *WSConn) {
	// Soft include of controlMessage on establish of websocket connection.
	controlMessage := &nats.Msg{
		Subject: H8SControlWebsocketEstablished,
		Reply:   wsConn.SubscribeSubject,
		Header:  nats.Header(wsConn.Headers),
	}
	controlMessage.Header.Add(H8SPublishSubjectHTTPHeaderName, wsConn.PublishSubject)

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

// cmConnectionClosed publishes a message on the control channel indicating that
// a websocket connection has been closed or cannot be written to.
func (h8s *H8Sproxy) cmConnectionClosed(secKey string) {
	wsConn := h8s.WSPool.Get(secKey)
	if wsConn == nil {
		return
	}

	controlMsg := &nats.Msg{
		Subject: H8SControlWebsocketClosed,
		Reply:   wsConn.SubscribeSubject,
		Header:  nats.Header(wsConn.Headers),
	}

	err := h8s.NATSConn.PublishMsg(controlMsg)
	if err != nil {
		slog.Error(
			"unable to publish control message",
			"message", controlMsg)
	}
	slog.Info(
		"connection closed, published control message",
		"message", controlMsg,
	)
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

	body, err := io.ReadAll(req.Body)
	if err != nil {
		slog.Error("failed to read request body", "error", err)
	}
	defer req.Body.Close()
	msg.Data = body

	// Add propagation of nats subject as X-H8S-Subject header
	// This can be used to inform downstream business logic that
	// does not do any direct NATS communication.
	msg.Header.Add(H8SPublishSubjectHTTPHeaderName, msg.Subject)
	// Propagate the original query string as a header
	msg.Header.Add(H8SOriginalQueryHTTPHeaderName, req.URL.RawQuery)
	msg.Header.Add(H8SReplySubjectHTTPHeaderName, msg.Reply)
	msg.Header.Add(
		H8SConnectionCloseSubjectHeaderName,
		fmt.Sprintf("%v.%v", H8SControlConnectionClosedSubjectPrefix, nuid.Next()))

	return msg
}

// CopyHeadersOnce copies relevant headers from a NATS reply message
// to the http.ResponseWriter and writes the status code immediately.
func copyHeadersOnce(res http.ResponseWriter, h nats.Header) {
	statusCode := http.StatusOK

	if s := h.Get("Status-Code"); s != "" {
		if v, convErr := strconv.Atoi(strings.TrimSpace(s)); convErr == nil && v >= 100 && v <= 999 {
			statusCode = v
		}
	} else if s := h.Get("Status"); s != "" {
		if v, convErr := strconv.Atoi(strings.TrimSpace(s)); convErr == nil && v >= 100 && v <= 999 {
			statusCode = v
		}
	}

	// Make headers canonical and assign values
	for k, vals := range h {
		kn := http.CanonicalHeaderKey(k)
		for i, v := range vals {
			if i == 0 {
				res.Header().Set(kn, v)
			} else {
				res.Header().Add(kn, v)
			}
		}
	}

	if te := h.Get("Transfer-Encoding"); strings.EqualFold(te, "chunked") {
		h.Del("Content-Length")
	} else if cl := h.Get("Content-Length"); cl != "" {
		res.Header().Set("Content-Length", cl)
	}

	res.WriteHeader(statusCode)
}
