package subjectmapper

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const (
	SubjectPrefix   = "h8s"
	InboxPrefix     = "_INBOX.h8s"
	WebScoketMethod = "ws"
)

type SubjectMap struct {
	Request *http.Request
	// Request path from the HTTP request sanitized for used in NATS Subject
	Path string
	// Reversed host for NATS Subject. IP's will not be reversed.
	Host          string
	SubjectPrefix string
	InboxPrefix   string
}

type SubjectMapOption func(*SubjectMap)

type SubjectMapper interface {
	PublishSubject() string
	InboxSubjectPrefix() string
	ReversedHost() string
	PathSegments() string
}

func WithSubjectPrefix(prefix string) SubjectMapOption {
	return func(h *SubjectMap) {
		h.SubjectPrefix = prefix
	}
}

func WithInboxPrefix(prefix string) SubjectMapOption {
	return func(h *SubjectMap) {
		h.InboxPrefix = prefix
	}
}

func NewSubjectMap(req *http.Request, options ...SubjectMapOption) *SubjectMap {
	sm := &SubjectMap{
		Request:       req,
		SubjectPrefix: SubjectPrefix,
		InboxPrefix:   InboxPrefix,
	}

	for _, opt := range options {
		opt(sm)
	}

	// Process Host to reverse it. Host names are reverse
	// IP octet is kept.
	// Port is dropped, not relevant in NATS.
	sm.processHost(req.Host)

	// Process Path and build part of a NATS subject.
	sm.processPath(req.URL.Path)

	return sm
}

func NewWebSocketMap(url string, options ...SubjectMapOption) *SubjectMap {
	sm := &SubjectMap{
		Request:       nil,
		SubjectPrefix: SubjectPrefix,
		InboxPrefix:   InboxPrefix,
	}

	for _, opt := range options {
		opt(sm)
	}

	// Create a synthetic request from the websocket url.
	sreq, err := sm.processWebSocketURL(url)
	if err != nil {
		slog.Error("Failed to process WebSocket URL", "error", err)
	}

	sm.Request = sreq
	sm.processHost(sreq.Host)
	sm.processPath(sreq.URL.Path)

	return sm
}

func (sm *SubjectMap) processWebSocketURL(urlStr string) (*http.Request, error) {
	url, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}
	req := &http.Request{
		URL:    url,
		Method: "ws",
		Host:   url.Host,
	}

	return req, nil
}

func (sm *SubjectMap) PublishSubject() string {
	subject := strings.Join([]string{
		sm.SubjectPrefix,
		sm.Request.URL.Scheme,
		sm.Request.Method,
		sm.Host,
		sm.Path,
	},
		".")

	return subject
}

func (sm *SubjectMap) WebSocketPublishSubject() string {
	subject := strings.Join([]string{
		sm.SubjectPrefix,
		WebScoketMethod,
		WebScoketMethod,
		sm.Host,
		sm.Path,
	},
		".")

	return subject
}

func (sm *SubjectMap) InboxSubjectPrefix() string {
	// websocket prefix based on unique key for connection
	if sm.Request.Header.Get("Sec-Websocket-Key") != "" {
		inbox := strings.Join(
			[]string{
				sm.InboxPrefix,
				sm.Request.Header.Get("Sec-Websocket-Key"),
			}, ".")
		return inbox
	}

	// Non websocket prefix
	return strings.Join(
		[]string{
			sm.InboxPrefix,
		}, ".")
}

func (sm *SubjectMap) ReversedHost() string {
	return sm.Host
}

func (sm *SubjectMap) PathSegments() string {
	return sm.Path
}

func (sm *SubjectMap) processHost(host string) {
	//    Attempt to parse out a port if present
	hostOnly, _, err := net.SplitHostPort(host)
	if err != nil {
		// net.SplitHostPort fails if there's no port or if the format is invalid.
		// In that case, just assume the entire string is host-only.
		hostOnly = host
	}

	// Remove surrounding brackets if bracketed IPv6 (e.g., "[2001:db8::1]").
	hostOnly = strings.TrimPrefix(hostOnly, "[")
	hostOnly = strings.TrimSuffix(hostOnly, "]")

	// Check if it’s a valid IP (IPv4 or IPv6).
	if parsed := net.ParseIP(hostOnly); parsed != nil {
		// It's an IP, so replace ":" with "_" that may be used in NATS subjects
		sm.Host = strings.ReplaceAll(hostOnly, ":", "_")
		return
	}

	// Otherwise, it's a domain. Reverse "foo.bar.com" -> "com.bar.foo".
	parts := strings.Split(hostOnly, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	sm.Host = strings.Join(parts, ".")
}

func isIP(host string) bool {
	// Attempt to parse out a port if present (e.g., "127.0.0.1:8080", "[::1]:443").
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// For bracketed IPv6 addresses (with or without port), remove surrounding brackets.
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	// Now check if it's a valid IPv4 or IPv6 address.
	ip := net.ParseIP(host)
	return ip != nil
}

func (sm *SubjectMap) processPath(path string) {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		decoded = path // fallback to original if malformed
	}
	segments := strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' })
	safeSegments := make([]string, len(segments))
	for i, s := range segments {
		safeSegments[i] = strings.ReplaceAll(s, ".", "_")
	}
	if len(safeSegments) == 0 {
		sm.Path = "root"
	}
	sm.Path = strings.Join(safeSegments, ".")
}
