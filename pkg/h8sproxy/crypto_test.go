package h8sproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestProxySealsRequestAndOpensResponse(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	proxyKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer proxyKey.Wipe()
	responderKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer responderKey.Wipe()

	interestTracker := tracker.NewInterestTracker(nc,
		tracker.WithInterestSubject(H8SInterestControlSubject))
	require.NoError(t, interestTracker.Run())

	reqURL, err := url.Parse("http://example.com/secret")
	require.NoError(t, err)
	subject := subjectmapper.NewSubjectMap(&http.Request{
		Method: "GET",
		Host:   "example.com",
		URL:    reqURL,
	}).PublishSubject()

	publishInterest(t, nc, interestTracker, tracker.Interest{
		Host:       "example.com",
		Path:       "/secret",
		Method:     "GET",
		PublicKey:  responderKey.PublicKey(),
		InstanceID: "responder-1",
	})

	_, err = nc.Subscribe(subject, func(msg *nats.Msg) {
		from := msg.Header.Get(H8SFromHeaderName)
		require.Equal(t, proxyKey.PublicKey(), from, "h8sd public key must travel with the request")
		require.NotEqual(t, "attacker-key", from, "client-supplied X-H8s-From must be stripped")

		plaintext, err := responderKey.Open(msg.Data, from)
		require.NoError(t, err)
		require.Equal(t, []byte("request body"), plaintext)

		sealed, err := responderKey.Seal([]byte("response body"), from)
		require.NoError(t, err)
		header := nats.Header{}
		header.Set("Status", "200")
		header.Set("Status-Code", "200")
		header.Set("Content-Length", strconv.Itoa(len("response body")))
		require.NoError(t, nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    sealed,
			Header:  header,
		}))
	})
	require.NoError(t, err)

	proxy := NewH8Sproxy(nc,
		WithInterestTracker(interestTracker),
		WithCryptoKey(proxyKey),
	)
	handler := http.HandlerFunc(proxy.Handler)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/secret", strings.NewReader("request body"))
	req.Header.Set(H8SFromHeaderName, "attacker-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body, err := io.ReadAll(w.Result().Body)
	require.NoError(t, err)
	require.Equal(t, "response body", string(body))
}

func TestProxyAcceptsLegacyInterestWithoutPublicKey(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	interestTracker := tracker.NewInterestTracker(nc,
		tracker.WithInterestSubject(H8SInterestControlSubject))
	require.NoError(t, interestTracker.Run())

	// A legacy-style payload: no "pub" field at all. The current required
	// "instance" field is still present.
	legacyPayload := []byte(`{"host":"legacy.example","path":"/old","method":"GET","instance":"legacy-1"}`)
	require.NoError(t, nc.Publish(H8SInterestControlSubject+".register.example.legacy", legacyPayload))
	require.NoError(t, nc.FlushTimeout(time.Second))

	var resolution tracker.Resolution
	require.Eventually(t, func() bool {
		resolution = interestTracker.ResolveRoute("legacy.example", "/old", "GET")
		return resolution.Found
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, resolution.PublicKey)
	require.False(t, resolution.Conflict)

	reqURL, err := url.Parse("http://legacy.example/old")
	require.NoError(t, err)
	subject := subjectmapper.NewSubjectMap(&http.Request{
		Method: "GET",
		Host:   "legacy.example",
		URL:    reqURL,
	}).PublishSubject()

	_, err = nc.Subscribe(subject, func(msg *nats.Msg) {
		require.Empty(t, msg.Header.Get(H8SFromHeaderName), "plaintext interest must not set X-H8s-From")
		require.Equal(t, []byte("legacy request"), msg.Data)

		header := nats.Header{}
		header.Set("Status", "200")
		header.Set("Status-Code", "200")
		header.Set("Content-Length", strconv.Itoa(len("legacy response")))
		require.NoError(t, nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    []byte("legacy response"),
			Header:  header,
		}))
	})
	require.NoError(t, err)

	proxy := NewH8Sproxy(nc,
		WithInterestOnly(),
		WithInterestTracker(interestTracker),
	)
	handler := http.HandlerFunc(proxy.Handler)

	req := httptest.NewRequest(http.MethodGet, "http://legacy.example/old", strings.NewReader("legacy request"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body, err := io.ReadAll(w.Result().Body)
	require.NoError(t, err)
	require.Equal(t, "legacy response", string(body))
}

func TestProxyWithoutInterestTrackerDoesNotEncrypt(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	responderKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer responderKey.Wipe()

	// Even when a responder advertises a key, a proxy without a tracker does
	// not discover it and therefore must forward plaintext.
	interestData, err := json.Marshal(tracker.Interest{
		Host: "untracked.example", Path: "/x", Method: "GET",
		PublicKey: responderKey.PublicKey(), InstanceID: "a",
	})
	require.NoError(t, err)
	require.NoError(t, nc.Publish(H8SInterestControlSubject+".register.example.untracked", interestData))
	require.NoError(t, nc.FlushTimeout(time.Second))

	reqURL, err := url.Parse("http://untracked.example/x")
	require.NoError(t, err)
	subject := subjectmapper.NewSubjectMap(&http.Request{
		Method: "GET",
		Host:   "untracked.example",
		URL:    reqURL,
	}).PublishSubject()

	_, err = nc.Subscribe(subject, func(msg *nats.Msg) {
		require.Empty(t, msg.Header.Get(H8SFromHeaderName), "proxy without a tracker must not seal requests")
		require.Equal(t, []byte("plain request"), msg.Data)

		header := nats.Header{}
		header.Set("Status", "200")
		header.Set("Status-Code", "200")
		header.Set("Content-Length", strconv.Itoa(len("plain response")))
		require.NoError(t, nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    []byte("plain response"),
			Header:  header,
		}))
	})
	require.NoError(t, err)

	proxy := NewH8Sproxy(nc)
	handler := http.HandlerFunc(proxy.Handler)

	req := httptest.NewRequest(http.MethodGet, "http://untracked.example/x", strings.NewReader("plain request"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body, err := io.ReadAll(w.Result().Body)
	require.NoError(t, err)
	require.Equal(t, "plain response", string(body))
}

func TestProxyRejectsConflictingInterestKeys(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	proxyKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer proxyKey.Wipe()
	keyA, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer keyA.Wipe()
	keyB, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer keyB.Wipe()

	interestTracker := tracker.NewInterestTracker(nc,
		tracker.WithInterestSubject(H8SInterestControlSubject))
	require.NoError(t, interestTracker.Run())

	publishInterest(t, nc, interestTracker, tracker.Interest{
		Host: "conflict.example", Path: "/x", Method: "GET",
		PublicKey: keyA.PublicKey(), InstanceID: "a",
	})
	publishInterest(t, nc, interestTracker, tracker.Interest{
		Host: "conflict.example", Path: "/x", Method: "GET",
		PublicKey: keyB.PublicKey(), InstanceID: "b",
	})

	proxy := NewH8Sproxy(nc,
		WithInterestOnly(),
		WithInterestTracker(interestTracker),
		WithCryptoKey(proxyKey),
	)
	handler := http.HandlerFunc(proxy.Handler)

	req := httptest.NewRequest(http.MethodGet, "http://conflict.example/x", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)
}

func TestProxyFailsClosedWhenSealedRouteHasNoProxyKey(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	responderKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer responderKey.Wipe()

	interestTracker := tracker.NewInterestTracker(nc,
		tracker.WithInterestSubject(H8SInterestControlSubject))
	require.NoError(t, interestTracker.Run())
	publishInterest(t, nc, interestTracker, tracker.Interest{
		Host: "sealed.example", Path: "/x", Method: "GET",
		PublicKey: responderKey.PublicKey(), InstanceID: "a",
	})

	proxy := NewH8Sproxy(nc, WithInterestOnly(), WithInterestTracker(interestTracker))
	// Simulate an initialization failure; NewH8Sproxy normally generates a key.
	proxy.CryptoKey = nil
	handler := http.HandlerFunc(proxy.Handler)

	req := httptest.NewRequest(http.MethodGet, "http://sealed.example/x", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)
}

func publishInterest(t *testing.T, nc *nats.Conn, tracker *tracker.InterestTracker, interest tracker.Interest) {
	t.Helper()
	data, err := json.Marshal(interest)
	require.NoError(t, err)
	reversedHost := subjectmapper.ReverseHostname(interest.Host)
	require.NoError(t, nc.Publish(H8SInterestControlSubject+".register."+reversedHost, data))
	require.NoError(t, nc.FlushTimeout(time.Second))
	require.Eventually(t, func() bool {
		return tracker.ResolveRoute(interest.Host, interest.Path, interest.Method).Found
	}, time.Second, 10*time.Millisecond)
}

func TestProxyWebSocketSealsFrames(t *testing.T) {
	ns := startEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	proxyKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer proxyKey.Wipe()
	responderKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer responderKey.Wipe()

	interestTracker := tracker.NewInterestTracker(nc,
		tracker.WithInterestSubject(H8SInterestControlSubject))
	require.NoError(t, interestTracker.Run())

	proxy := NewH8Sproxy(nc,
		WithInterestTracker(interestTracker),
		WithCryptoKey(proxyKey),
		WithRequestTimeout(5*time.Second),
	)
	srv := httptest.NewServer(http.HandlerFunc(proxy.handleWebSocket))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/test/foo"
	u, err := url.Parse(wsURL)
	require.NoError(t, err)
	publishInterest(t, nc, interestTracker, tracker.Interest{
		Host:       u.Host,
		Path:       u.Path,
		Method:     subjectmapper.WebSocketMethod,
		PublicKey:  responderKey.PublicKey(),
		InstanceID: "ws-responder",
	})

	sm := subjectmapper.NewWebSocketMap(wsURL)
	done := make(chan struct{})
	var (
		gotFrom  string
		gotPlain []byte
	)
	sub, err := nc.Subscribe(sm.WebSocketPublishSubject(), func(msg *nats.Msg) {
		defer close(done)
		gotFrom = msg.Header.Get(H8SFromHeaderName)
		if gotFrom != proxyKey.PublicKey() {
			t.Errorf("unexpected sender key: %q", gotFrom)
			return
		}
		plain, openErr := responderKey.Open(msg.Data, gotFrom)
		if openErr != nil {
			t.Errorf("open WebSocket frame: %v", openErr)
			return
		}
		gotPlain = plain
		sealed, sealErr := responderKey.Seal([]byte("pong"), gotFrom)
		if sealErr != nil {
			t.Errorf("seal WebSocket reply: %v", sealErr)
			return
		}
		if pubErr := nc.PublishMsg(&nats.Msg{Subject: msg.Reply, Data: sealed}); pubErr != nil {
			t.Errorf("publish WebSocket reply: %v", pubErr)
		}
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	ws, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{"X-WS-Test": []string{"crypto"}})
	require.NoError(t, err)
	defer ws.Close()

	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte("ping")))
	_, response, err := ws.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "pong", string(response))

	<-done
	require.Equal(t, proxyKey.PublicKey(), gotFrom)
	require.Equal(t, []byte("ping"), gotPlain)
}
