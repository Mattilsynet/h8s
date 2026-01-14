package h8sproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestProxySecurityFeatures(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	t.Run("HostFilter", func(t *testing.T) {
		// Proxy that only allows "allowed.com"
		proxy := NewH8Sproxy(nc, WithHostFilter("allowed.com"))

		handler := http.HandlerFunc(proxy.Handler)

		// Case 1: Denied Host
		req := httptest.NewRequest("GET", "http://denied.com/foo", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)

		// Case 2: Allowed Host
		req = httptest.NewRequest("GET", "http://allowed.com/foo", nil)
		// We expect 502 Bad Gateway because NATS is not subscribed, but that means it passed the filter
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusServiceUnavailable, w.Code) // passed filter, failed upstream (no responders)

		// Case 3: Allowed via X-Forwarded-Host
		req = httptest.NewRequest("GET", "http://internal-lb/foo", nil)
		req.Header.Set("X-Forwarded-Host", "allowed.com")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusServiceUnavailable, w.Code) // passed filter
	})

	t.Run("RequestTimeout", func(t *testing.T) {
		// Short timeout for testing
		shortTimeout := 100 * time.Millisecond
		proxy := NewH8Sproxy(nc, WithRequestTimeout(shortTimeout))
		handler := http.HandlerFunc(proxy.Handler)

		// Subscribe to everything to prevent "no responders" but do NOT reply
		sub, err := nc.Subscribe("h8s.>", func(msg *nats.Msg) {
			// Do nothing, just ack existence basically by being here (NATS server sees interest)
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()

		req := httptest.NewRequest("GET", "http://timeout.test/timeout", nil)
		w := httptest.NewRecorder()

		start := time.Now()
		handler.ServeHTTP(w, req)
		duration := time.Since(start)

		// Check basic timing
		if duration > 1*time.Second {
			t.Errorf("Request took too long: %v", duration)
		}

		// If timeout happens, we generally expect 504 Gateway Timeout
		// But currently implementation returns nothing, so likely 200 OK.
		// Let's assert what we WANT (504) and see it fail.
		require.Equal(t, http.StatusGatewayTimeout, w.Code)
	})
}

func TestInterestOnlyMode_AllowsRegisteredInterest(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	interestSubject := "h8s.control.interest"
	interestTracker := tracker.NewInterestTracker(nc, tracker.WithInterestSubject(interestSubject))
	require.NoError(t, interestTracker.Run())

	proxy := NewH8Sproxy(nc, WithInterestOnly(), WithInterestTracker(interestTracker))
	handler := http.HandlerFunc(proxy.Handler)

	req := httptest.NewRequest(http.MethodGet, "http://allowed.com/ping", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	interest := tracker.Interest{
		Host:   "allowed.com",
		Path:   "/ping",
		Method: "GET",
	}
	data, err := json.Marshal(interest)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(interestSubject, data))
	require.NoError(t, nc.FlushTimeout(1*time.Second))

	require.Eventually(t, func() bool {
		interestTracker.Interests.RLock()
		defer interestTracker.Interests.RUnlock()
		_, ok := interestTracker.Interests.InterestMap[interest.Id()]
		return ok
	}, time.Second, 10*time.Millisecond)

	req = httptest.NewRequest(http.MethodGet, "http://allowed.com/ping", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.NotEqual(t, http.StatusNotFound, w.Code)

	req = httptest.NewRequest(http.MethodGet, "http://denied.com/ping", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestWebSocketOriginFilter(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	t.Run("DefaultAllowsAll", func(t *testing.T) {
		proxy := NewH8Sproxy(nc)
		srv := httptest.NewServer(http.HandlerFunc(proxy.handleWebSocket))
		t.Cleanup(srv.Close)

		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/test"
		dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment}
		headers := http.Header{"Origin": []string{"https://evil.example.com"}}

		conn, _, err := dialer.Dial(wsURL, headers)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("EnforcedAllowsOnlyConfiguredOrigins", func(t *testing.T) {
		proxy := NewH8Sproxy(nc, WithAllowedOrigins("https://good.example.com"))
		srv := httptest.NewServer(http.HandlerFunc(proxy.handleWebSocket))
		t.Cleanup(srv.Close)

		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/test"
		dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment}

		headers := http.Header{"Origin": []string{"https://good.example.com"}}
		conn, _, err := dialer.Dial(wsURL, headers)
		require.NoError(t, err)
		require.NoError(t, conn.Close())

		headers = http.Header{"Origin": []string{"https://evil.example.com"}}
		_, _, err = dialer.Dial(wsURL, headers)
		require.Error(t, err)
	})
}

func TestInterestSubjectAlignment(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	interestTracker := tracker.NewInterestTracker(nc, tracker.WithInterestSubject(H8SInterestControlSubject))
	require.NoError(t, interestTracker.Run())

	proxy := NewH8Sproxy(nc, WithInterestOnly(), WithInterestTracker(interestTracker))
	handler := http.HandlerFunc(proxy.Handler)

	interest := tracker.Interest{Host: "aligned.com", Path: "/ping", Method: "GET"}
	payload, err := json.Marshal(interest)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(H8SInterestControlSubject, payload))
	require.NoError(t, nc.FlushTimeout(1*time.Second))

	require.Eventually(t, func() bool {
		interestTracker.Interests.RLock()
		defer interestTracker.Interests.RUnlock()
		_, ok := interestTracker.Interests.InterestMap[interest.Id()]
		return ok
	}, time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "http://aligned.com/ping", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.NotEqual(t, http.StatusNotFound, w.Code)
}
