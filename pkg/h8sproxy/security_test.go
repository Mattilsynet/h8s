package h8sproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		require.Equal(t, http.StatusBadGateway, w.Code) // passed filter, failed upstream

		// Case 3: Allowed via X-Forwarded-Host
		req = httptest.NewRequest("GET", "http://internal-lb/foo", nil)
		req.Header.Set("X-Forwarded-Host", "allowed.com")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadGateway, w.Code) // passed filter
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
