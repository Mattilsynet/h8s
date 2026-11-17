package h8sreverse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/interest"
	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

// ReverseProxy forwards NATS requests to HTTP and publishes back.
// It can subscribe to a filtered set of subjects based on the subjectmapper rules.

type wsBackendConn struct {
	conn *websocket.Conn
	// recipient is h8sd's curve public key. Backend frames are sealed to it.
	recipient string
}

type ReverseProxy struct {
	nats         *nats.Conn
	client       *http.Client
	wsConns      sync.Map // map[string]*wsBackendConn (key is Reply subject)
	activeSubs   sync.Map // map[host][]*nats.Subscription
	interestStop sync.Map // map[host]context.CancelFunc
	Resolver     BackendResolver
	FilterHost   string // Optional: Only handle requests for this host (original hostname)
	QueueGroup   string // Optional: NATS Queue Group for load balancing
	bufferPool   *sync.Pool
	httpRequests chan struct{} // Admission is bounded before a worker is started.

	// CryptoKey is this responder's curve keypair. With no QueueGroup it is
	// safe to be ephemeral. With a QueueGroup every replica must configure the
	// same seed; otherwise this instance does not advertise a key.
	CryptoKey              *msgcrypto.Key
	cryptoKeyExplicit      bool
	InstanceID             string
	InterestPublishSubject string
}

const defaultMaxConcurrentHTTPRequests = 128

type Option func(*ReverseProxy)

// WithCryptoKey configures the responder keypair. When a queue group is in
// use, all replicas must pass the same key/seed.
func WithCryptoKey(key *msgcrypto.Key) Option {
	return func(r *ReverseProxy) {
		r.CryptoKey = key
		r.cryptoKeyExplicit = true
	}
}

// WithInterestPublishSubject overrides the base interest subject.
func WithInterestPublishSubject(subject string) Option {
	return func(r *ReverseProxy) {
		r.InterestPublishSubject = subject
	}
}

// NewReverseProxy creates a proxy with a default HTTP client.
func NewReverseProxy(nc *nats.Conn, opts ...Option) *ReverseProxy {
	// Tune HTTP Transport for high throughput
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	proxy := &ReverseProxy{
		nats:                   nc,
		httpRequests:           make(chan struct{}, defaultMaxConcurrentHTTPRequests),
		InstanceID:             nuid.Next(),
		InterestPublishSubject: h8sproxy.H8SInterestControlSubject,
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			// A reverse proxy must return redirects to the browser instead of
			// following them as an HTTP client.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		bufferPool: &sync.Pool{
			New: func() interface{} {
				// 32KB buffer for reasonably efficient I/O
				return make([]byte, 32*1024)
			},
		},
	}
	for _, opt := range opts {
		opt(proxy)
	}
	if proxy.CryptoKey == nil {
		key, err := msgcrypto.Generate()
		if err != nil {
			slog.Error("failed to generate reverse proxy crypto key", "error", err)
		} else {
			proxy.CryptoKey = key
		}
	}
	return proxy
}

// interestPublicKey returns the key to advertise. An ephemeral key is unsafe
// when a queue group is configured because another replica may receive the
// request. In that case the operator must provide a shared seed explicitly.
func (r *ReverseProxy) interestPublicKey() string {
	if r.CryptoKey == nil {
		return ""
	}
	if r.QueueGroup != "" && !r.cryptoKeyExplicit {
		slog.Warn("queue group is set but no shared crypto seed was configured; advertising no public key",
			"queue_group", r.QueueGroup)
		return ""
	}
	return r.CryptoKey.PublicKey()
}

func (r *ReverseProxy) newInterestPublisher(host string) *interest.Publisher {
	return &interest.Publisher{
		NATSConn: r.nats,
		Subject:  r.InterestPublishSubject,
		Entries: func() []tracker.Interest {
			return []tracker.Interest{{
				Host:       host,
				Path:       "*",
				Method:     "*",
				PublicKey:  r.interestPublicKey(),
				InstanceID: r.InstanceID,
			}}
		},
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
	handler := r.messageHandler(ctx)
	// Data subjects use queue groups for load balancing
	dataPatterns := []string{
		subjectmapper.SubjectPrefix + ".http.*.*.>",
		subjectmapper.SubjectPrefix + ".http.*.*", // Handle root path (no path segments)
		subjectmapper.SubjectPrefix + ".ws.ws.*.>",
		subjectmapper.SubjectPrefix + ".ws.ws.*", // Handle root path (no path segments)
	}
	for _, pat := range dataPatterns {
		var err error
		if r.QueueGroup != "" {
			_, err = r.nats.QueueSubscribe(pat, r.QueueGroup, handler)
		} else {
			_, err = r.nats.Subscribe(pat, handler)
		}
		if err != nil {
			return err
		}
	}

	// Control subjects: NEVER use queue groups. Each h8srd instance needs
	// its own backend WS connection, so every instance must receive every
	// control message for the hosts it serves.
	controlPatterns := []string{
		"h8s.control.ws.conn.established.>",
		"h8s.control.ws.conn.closed.>",
	}
	for _, pat := range controlPatterns {
		if _, err := r.nats.Subscribe(pat, handler); err != nil {
			return err
		}
	}
	return nil
}

// SubscribeForHost subscribes to subjects specific to a given hostname.
func (r *ReverseProxy) SubscribeForHost(ctx context.Context, host string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	handler := r.messageHandler(ctx)
	// Reverse the host using subjectmapper logic
	req, err := http.NewRequest("GET", "http://"+host, nil)
	if err != nil {
		return err
	}
	sm := subjectmapper.NewSubjectMap(req)
	reversedHost := sm.ReversedHost()

	// Data subjects — use queue groups for load balancing
	dataPatterns := []string{
		subjectmapper.SubjectPrefix + ".http.*." + reversedHost + ".>",
		subjectmapper.SubjectPrefix + ".http.*." + reversedHost,
		subjectmapper.SubjectPrefix + ".ws.ws." + reversedHost + ".>",
		subjectmapper.SubjectPrefix + ".ws.ws." + reversedHost,
	}

	var subs []*nats.Subscription
	for _, pat := range dataPatterns {
		var sub *nats.Subscription
		var err error
		if r.QueueGroup != "" {
			sub, err = r.nats.QueueSubscribe(pat, r.QueueGroup, handler)
		} else {
			sub, err = r.nats.Subscribe(pat, handler)
		}
		if err != nil {
			// Cleanup any successful subscriptions on failure
			for _, s := range subs {
				s.Unsubscribe()
			}
			return err
		}
		subs = append(subs, sub)
	}

	// Control subjects — NEVER use queue groups. Each h8srd instance needs
	// its own backend WS connection, so every instance must receive every
	// control message for the hosts it serves.
	controlPatterns := []string{
		"h8s.control.ws.conn.established." + reversedHost,
		"h8s.control.ws.conn.closed." + reversedHost,
	}
	for _, pat := range controlPatterns {
		sub, err := r.nats.Subscribe(pat, handler)
		if err != nil {
			for _, s := range subs {
				s.Unsubscribe()
			}
			return err
		}
		subs = append(subs, sub)
	}

	// Store subscriptions for later cleanup
	if existing, ok := r.activeSubs.Load(host); ok {
		current := existing.([]*nats.Subscription)
		r.activeSubs.Store(host, append(current, subs...))
	} else {
		r.activeSubs.Store(host, subs)
	}

	// Advertise a wildcard route for this host. h8sreverse is a first-class
	// responder: it may serve any path/method for the host.
	if previous, ok := r.interestStop.Load(host); ok {
		previous.(context.CancelFunc)()
	}
	publisherCtx, stop := context.WithCancel(ctx)
	r.interestStop.Store(host, stop)
	go r.newInterestPublisher(host).Run(publisherCtx)

	return nil
}

// UnsubscribeForHost removes subscriptions for a given hostname.
func (r *ReverseProxy) UnsubscribeForHost(host string) error {
	if stop, ok := r.interestStop.Load(host); ok {
		stop.(context.CancelFunc)()
		r.interestStop.Delete(host)
	}
	// Publish unregister before removing data subscriptions so the tracker
	// stops routing to this host immediately.
	r.newInterestPublisher(host).Unregister()

	val, ok := r.activeSubs.Load(host)
	if !ok {
		return nil
	}

	subs := val.([]*nats.Subscription)
	var lastErr error
	for _, sub := range subs {
		if err := sub.Unsubscribe(); err != nil {
			slog.Error("Failed to unsubscribe", "host", host, "subject", sub.Subject, "error", err)
			lastErr = err
		}
	}

	r.activeSubs.Delete(host)
	return lastErr
}

// NATS invokes a subscription callback serially. Keeping an HTTP long poll or
// streamed body in that callback makes every matching request wait behind it.
// Dispatch HTTP only, with bounded admission shared across hosts/subscriptions;
// WS control/data stays synchronous to retain its existing ordering.
func (r *ReverseProxy) messageHandler(ctx context.Context) nats.MsgHandler {
	if ctx == nil {
		ctx = context.Background()
	}
	return func(msg *nats.Msg) {
		parts := strings.SplitN(msg.Subject, ".", 3)
		if len(parts) < 2 || parts[1] != "http" {
			r.handleMsg(ctx, msg)
			return
		}
		if ctx.Err() != nil {
			r.publishError(msg, http.StatusServiceUnavailable, "Reverse proxy is shutting down")
			return
		}
		select {
		case r.httpRequests <- struct{}{}:
			go func() {
				defer func() { <-r.httpRequests }()
				r.handleMsg(ctx, msg)
			}()
		default:
			r.publishError(msg, http.StatusServiceUnavailable, "Reverse proxy HTTP concurrency limit reached")
		}
	}
}

func (r *ReverseProxy) handleMsg(ctx context.Context, msg *nats.Msg) {
	// Control messages: subjects now include .<reversed-host> suffix,
	// so use prefix matching instead of exact equality.
	if strings.HasPrefix(msg.Subject, "h8s.control.ws.conn.established") {
		r.handleControlEstablished(msg)
		return
	}
	if strings.HasPrefix(msg.Subject, "h8s.control.ws.conn.closed") {
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

	var host string
	var path string
	var urlStr string
	var u *url.URL
	var err error

	hostHeader := msg.Header.Get("X-H8s-Original-Host")
	if hostHeader == "" {
		hostHeader = msg.Header.Get("Host")
	}
	originalPath := msg.Header.Get("X-H8s-Original-Path")
	originalQuery := msg.Header.Get("X-H8s-Original-Query")
	originalProto := msg.Header.Get("X-H8s-Original-Proto")
	from := msg.Header.Get(h8sproxy.H8SFromHeaderName)

	body := msg.Data
	if from != "" {
		if r.CryptoKey == nil {
			slog.Error("received sealed request but reverse proxy has no crypto key", "subject", msg.Subject)
			r.publishError(msg, http.StatusInternalServerError, "encryption is not configured")
			return
		}
		plaintext, openErr := r.CryptoKey.Open(body, from)
		if openErr != nil {
			slog.Error("failed to open request body", "error", openErr, "subject", msg.Subject)
			r.publishError(msg, http.StatusBadRequest, "invalid encrypted request")
			return
		}
		body = plaintext
	}

	if hostHeader != "" {
		// Case 1: We have the Host header. This is the happy path.
		host = hostHeader

		// Calculate host segments to find where path starts
		reversedHost := subjectmapper.ReverseHostname(host)
		hostSegments := strings.Split(reversedHost, ".")
		pathStartIndex := 3 + len(hostSegments)

		if len(parts) > pathStartIndex {
			path = strings.Join(parts[pathStartIndex:], "/")
		}

		if r.Resolver != nil {
			u, err = r.Resolver.Resolve(ctx, host, path)
			if err != nil {
				slog.Error("backend resolution failed", "host", host, "error", err)
				r.publishError(msg, 502, "Backend not found")
				return
			}
		}
	} else {
		// Case 2: No Host header.
		// We fallback to checking subject parts but avoid heuristic guessing loop.
		// Subject format: prefix.scheme.method.<reversed-host-parts>.<path-parts>
		// Without a clear delimiter or fixed host length, parsing is ambiguous if host/path parts vary.
		// However, if we assume standard 1-segment host (internal services) or try to resolve
		// against known hosts if Resolver supports it?
		// For now, we fallback to the "Legacy" assumption: Host is 1 part (parts[3]).
		// This is deterministic but limited.
		// If users want multi-segment hosts, they MUST provide Host header.

		if len(parts) > 3 {
			host = parts[3]
			if len(parts) > 4 {
				path = strings.Join(parts[4:], "/")
			}
		}

		if r.Resolver != nil {
			u, err = r.Resolver.Resolve(ctx, host, path)
			// If resolution fails, u is nil, fallthrough to direct proxy
			if err != nil {
				// slog.Debug("Resolver lookup failed for fallback host", "host", host)
			}
		}
	}

	if u == nil {
		// No backend found via Resolver (or no Resolver).
		// Fallback to direct proxying to the host extracted from subject.
		urlStr = scheme + "://" + host + "/" + path
		if r.Resolver != nil {
			// Only log this if we actually expected a resolver to work
			slog.Info("No resolver match, falling back to direct", "url", urlStr)
		}
	} else {
		// Use configured backend
		// Construct URL using backend scheme and host, but keep the path from the request

		// We need to merge paths carefully.
		// Resolver returned the base URL for the backend service (e.g. http://foo.svc.local)
		// path from NATS subject is the relative path requested.

		targetPath := path
		// Prepend backend path if it exists (e.g. context root)
		if u.Path != "" {
			if strings.HasSuffix(u.Path, "/") && strings.HasPrefix(targetPath, "/") {
				targetPath = u.Path + targetPath[1:]
			} else if !strings.HasSuffix(u.Path, "/") && !strings.HasPrefix(targetPath, "/") {
				targetPath = u.Path + "/" + targetPath
			} else {
				targetPath = u.Path + targetPath
			}
		}

		// Copy the URL to avoid mutating the resolved one
		uCopy := *u
		uCopy.Path = targetPath
		urlStr = uCopy.String()
	}

	requestURL, err := url.Parse(urlStr)
	if err != nil {
		slog.Error("parse request URL", "error", err)
		r.publishError(msg, 502, "Invalid backend URL")
		return
	}
	if originalPath != "" {
		restoredPath, pathErr := url.PathUnescape(originalPath)
		if pathErr != nil {
			slog.Error("invalid original path", "error", pathErr)
			r.publishError(msg, 400, "Invalid request path")
			return
		}
		if u != nil && u.Path != "" {
			restoredPath = joinURLPath(u.Path, restoredPath)
			originalPath = joinURLPath(u.EscapedPath(), originalPath)
		}
		requestURL.Path = restoredPath
		requestURL.RawPath = originalPath
	}
	requestURL.RawQuery = originalQuery

	req, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		slog.Error("new request", "error", err)
		return
	}
	for k, v := range msg.Header {
		for _, vv := range v {
			req.Header.Add(k, vv)
		}
	}
	removeHopByHopHeaders(req.Header)
	removeInternalHeaders(req.Header)
	if hostHeader != "" {
		req.Header.Set("X-Forwarded-Host", hostHeader)
	}
	// The backend may use virtual-host routing and must therefore receive the
	// resolved backend authority as Host. Keep the public authority separately
	// in X-Forwarded-Host.
	req.Host = requestURL.Host
	if originalProto == "" {
		originalProto = scheme
	}
	req.Header.Set("X-Forwarded-Proto", originalProto)
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

	responseHeaders := resp.Header.Clone()
	removeHopByHopHeaders(responseHeaders)
	header := nats.Header{}
	for k, v := range responseHeaders {
		for _, vv := range v {
			header.Add(k, vv)
		}
	}
	header.Set("Status", strconv.Itoa(resp.StatusCode))
	header.Set("Status-Code", strconv.Itoa(resp.StatusCode))
	// Remove Content-Length to ensure h8sproxy streams the response
	header.Del("Content-Length")

	bufPtr := r.bufferPool.Get().([]byte)
	defer r.bufferPool.Put(bufPtr)
	buf := bufPtr

	first := true

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			msgData := make([]byte, n)
			copy(msgData, buf[:n])

			chunkHeader := nats.Header(nil)
			if first {
				chunkHeader = header
				first = false
			}
			if err := r.publishResponse(msg.Reply, msgData, chunkHeader, from); err != nil {
				slog.Error("failed to publish response chunk", "error", err, "reply", msg.Reply)
				return
			}
		}

		if err != nil {
			if err == io.EOF {
				// Send an empty plaintext message to close the stream on the
				// h8sproxy side. If it is the first message, attach headers.
				finishHeader := nats.Header(nil)
				if first {
					finishHeader = header
				}
				if pubErr := r.publishResponse(msg.Reply, []byte{}, finishHeader, from); pubErr != nil {
					slog.Error("failed to publish response EOF", "error", pubErr, "reply", msg.Reply)
				}
			} else {
				slog.Error("read body error", "error", err)
				// Best effort close
				if pubErr := r.publishResponse(msg.Reply, []byte{}, nil, from); pubErr != nil {
					slog.Error("failed to publish response close", "error", pubErr, "reply", msg.Reply)
				}
			}
			break
		}
	}
}

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, header := range hopByHopHeaders {
		h.Del(header)
	}
}

func removeInternalHeaders(h http.Header) {
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "x-h8s-") {
			h.Del(key)
		}
	}
}

func joinURLPath(basePath, requestPath string) string {
	switch {
	case strings.HasSuffix(basePath, "/") && strings.HasPrefix(requestPath, "/"):
		return basePath + requestPath[1:]
	case !strings.HasSuffix(basePath, "/") && !strings.HasPrefix(requestPath, "/"):
		return basePath + "/" + requestPath
	default:
		return basePath + requestPath
	}
}

// publishResponse seals data when recipient is set, then publishes one NATS
// message. Content-Length headers, when present, describe plaintext length.
func (r *ReverseProxy) publishResponse(subject string, data []byte, header nats.Header, recipient string) error {
	if recipient != "" {
		if r.CryptoKey == nil {
			return errors.New("reverse proxy crypto key is not configured")
		}
		sealed, err := r.CryptoKey.Seal(data, recipient)
		if err != nil {
			return err
		}
		data = sealed
	}
	return r.nats.PublishMsg(&nats.Msg{Subject: subject, Data: data, Header: header})
}

func (r *ReverseProxy) publishError(msg *nats.Msg, code int, errStr string) {
	recipient := msg.Header.Get(h8sproxy.H8SFromHeaderName)
	body := []byte(errStr)

	header := nats.Header{}
	header.Set("Status", strconv.Itoa(code))
	header.Set("Status-Code", strconv.Itoa(code))
	// Error replies are finite single messages, not unterminated streams.
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))

	if err := r.publishResponse(msg.Reply, body, header, recipient); err != nil {
		slog.Error("failed to publish error response", "error", err, "reply", msg.Reply)
	}
}

func (r *ReverseProxy) handleControlEstablished(msg *nats.Msg) {
	recipient := msg.Header.Get(h8sproxy.H8SFromHeaderName)
	if recipient != "" && r.CryptoKey == nil {
		slog.Error("received sealed WebSocket control message but reverse proxy has no crypto key")
		return
	}

	// Filter by host if configured
	hostHeader := msg.Header.Get("X-H8s-Original-Host")
	if hostHeader == "" {
		hostHeader = msg.Header.Get("Host")
	}
	if r.FilterHost != "" && hostHeader != "" {
		if !strings.EqualFold(hostHeader, r.FilterHost) {
			return
		}
	}

	// Use Host header to determine backend URL
	host := hostHeader
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

	targetURL := &url.URL{Scheme: "ws", Host: host, Path: path}
	var resolvedBackend *url.URL
	if r.Resolver != nil {
		ctx := context.Background()
		resolved, err := r.Resolver.Resolve(ctx, host, path)
		if err != nil {
			slog.Error("handleControlEstablished: failed to resolve backend", "host", host, "error", err)
			return
		}

		if resolved != nil {
			resolvedBackend = resolved
			// Use configured backend
			scheme := "ws"
			if resolved.Scheme == "https" {
				scheme = "wss"
			}

			targetPath := path
			if resolved.Path != "" {
				if strings.HasSuffix(resolved.Path, "/") && strings.HasPrefix(targetPath, "/") {
					targetPath = resolved.Path + targetPath[1:]
				} else if !strings.HasSuffix(resolved.Path, "/") && !strings.HasPrefix(targetPath, "/") {
					targetPath = resolved.Path + "/" + targetPath
				} else {
					targetPath = resolved.Path + targetPath
				}
			}

			targetURL = &url.URL{Scheme: scheme, Host: resolved.Host, Path: targetPath}
		}
	}
	if originalPath := msg.Header.Get("X-H8s-Original-Path"); originalPath != "" {
		restoredPath, err := url.PathUnescape(originalPath)
		if err != nil {
			slog.Error("handleControlEstablished: invalid original path", "error", err)
			return
		}
		if resolvedBackend != nil && resolvedBackend.Path != "" {
			restoredPath = joinURLPath(resolvedBackend.Path, restoredPath)
			originalPath = joinURLPath(resolvedBackend.EscapedPath(), originalPath)
		}
		targetURL.Path = restoredPath
		targetURL.RawPath = originalPath
	}
	targetURL.RawQuery = msg.Header.Get("X-H8s-Original-Query")

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	// Forward headers using Add/Set but filtering specific connection headers
	headers := http.Header{}
	for k, v := range msg.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-h8s-") {
			continue
		}
		// Skip headers managed by Dial
		if k == "Host" || k == "Sec-Websocket-Key" || k == "Connection" || k == "Upgrade" || strings.HasPrefix(k, "Sec-Websocket-") {
			continue
		}
		// Case insensitive check for canonical keys if needed, but NATS headers match HTTP usually
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Sec-WebSocket-Key") || strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Upgrade") || strings.HasPrefix(strings.ToLower(k), "sec-websocket-") {
			continue
		}
		for _, vv := range v {
			headers.Add(k, vv)
		}
	}
	removeInternalHeaders(headers)
	// The browser Origin identifies the public h8s endpoint, while the backend
	// WebSocket handshake targets the resolved service. Rewrite it to the
	// backend origin so standard same-origin checks compare matching hosts.
	if headers.Get("Origin") != "" {
		originScheme := "http"
		if targetURL.Scheme == "wss" {
			originScheme = "https"
		}
		headers.Set("Origin", (&url.URL{Scheme: originScheme, Host: targetURL.Host}).String())
	}
	if hostHeader != "" {
		headers.Set("X-Forwarded-Host", hostHeader)
	}
	originalProto := msg.Header.Get("X-H8s-Original-Proto")
	if originalProto == "" {
		originalProto = "http"
	}
	headers.Set("X-Forwarded-Proto", originalProto)

	wsConn, _, err := dialer.Dial(targetURL.String(), headers)
	if err != nil {
		slog.Error("handleControlEstablished: failed to dial backend", "url", targetURL.Redacted(), "error", err)
		return
	}

	// Store connection mapping: ReplySubject -> backend connection + h8sd key.
	state := &wsBackendConn{conn: wsConn, recipient: recipient}
	r.wsConns.Store(msg.Reply, state)
	slog.Info("handleControlEstablished: connected", "url", targetURL.Redacted(), "reply", msg.Reply)

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
			if err := r.publishResponse(msg.Reply, message, nil, recipient); err != nil {
				slog.Error("failed to seal WebSocket frame", "error", err, "reply", msg.Reply)
				break
			}
		}
	}()
}

func (r *ReverseProxy) handleControlClosed(msg *nats.Msg) {
	slog.Info("handleControlClosed", "reply", msg.Reply)
	if val, ok := r.wsConns.Load(msg.Reply); ok {
		if state, ok := val.(*wsBackendConn); ok && state.conn != nil {
			state.conn.Close()
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
	state, ok := val.(*wsBackendConn)
	if !ok || state.conn == nil {
		return
	}

	data := msg.Data
	if state.recipient != "" {
		if from := msg.Header.Get(h8sproxy.H8SFromHeaderName); from != "" && from != state.recipient {
			slog.Error("handleWebSocketFrame: sender key does not match established session", "reply", msg.Reply)
			return
		}
		if r.CryptoKey == nil {
			slog.Error("handleWebSocketFrame: sealed frame but no crypto key configured", "reply", msg.Reply)
			return
		}
		plaintext, err := r.CryptoKey.Open(data, state.recipient)
		if err != nil {
			slog.Error("handleWebSocketFrame: failed to open frame", "error", err, "reply", msg.Reply)
			return
		}
		data = plaintext
	}

	err := state.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		slog.Error("handleWebSocketFrame: write error", "error", err)
		state.conn.Close()
		r.wsConns.Delete(msg.Reply)
	}
}
