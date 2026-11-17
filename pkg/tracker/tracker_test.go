package tracker

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/internal/natstest"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func newTestInterests() *Interests {
	return &Interests{routes: make(map[routeKey]map[string]*instanceInterest)}
}

func TestInterestId(t *testing.T) {
	tests := []struct {
		name     string
		interest Interest
		want     string
	}{
		{
			"simple",
			Interest{Host: "host1", Path: "/path", Method: "GET", InstanceID: "i1"},
			"host1:/path:GET",
		},
		{"trailing slash", Interest{Host: "host2", Path: "/", Method: "GET", InstanceID: "i2"}, "host2:/:GET"},
		{"host with port and path with colon", Interest{Host: "host:8080", Path: "/a:b", Method: "GET", InstanceID: "i3"}, "host:8080:/a:b:GET"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.interest.Id())
		})
	}
}

func TestInterestsAddAndEvict(t *testing.T) {
	it := newTestInterests()

	interest := &Interest{Host: "localhost", Path: "/ws", Method: "GET", InstanceID: "i1"}
	require.True(t, it.Add(interest), "first add should create a route")
	require.False(t, it.Add(interest), "refresh should not create a route")
	require.Equal(t, 1, it.RouteCount())
	require.Equal(t, 1, it.InstanceCount())

	removedRoutes := it.EvictStale(time.Now().Add(3 * time.Minute))
	require.Equal(t, 1, removedRoutes)
	require.Equal(t, 0, it.RouteCount())
	require.Equal(t, 0, it.InstanceCount())
}

func TestInterestsLegacyEmptyInstanceIDFallback(t *testing.T) {
	it := newTestInterests()
	interest := &Interest{Host: "localhost", Path: "/api", Method: "GET"}
	require.True(t, it.Add(interest))
	require.Equal(t, 1, it.RouteCount())
	require.Equal(t, 1, it.InstanceCount())
	require.Equal(t, Resolution{Found: true, Instances: 1}, it.Resolve("localhost", "/api", "GET"))

	require.True(t, it.Remove(&Interest{Host: "localhost", Path: "/api", Method: "GET"}))
	require.Equal(t, 0, it.RouteCount())
}

func TestInterestsLegacyUnregisterDoesNotRemoveNewInstance(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "localhost", Path: "/api", Method: "GET", InstanceID: "real"}))
	require.False(t, it.Remove(&Interest{Host: "localhost", Path: "/api", Method: "GET"}))
	require.True(t, it.Resolve("localhost", "/api", "GET").Found)
	require.Equal(t, 1, it.InstanceCount())
}

func TestInterestsRemoveOnlyMatchingInstance(t *testing.T) {
	it := newTestInterests()
	a := &Interest{Host: "localhost", Path: "/api", Method: "GET", InstanceID: "a"}
	b := &Interest{Host: "localhost", Path: "/api", Method: "GET", InstanceID: "b"}
	require.True(t, it.Add(a))
	require.False(t, it.Add(b))

	require.False(t, it.Remove(&Interest{Host: "localhost", Path: "/api", Method: "GET", InstanceID: "missing"}))
	require.Equal(t, 2, it.InstanceCount())

	require.False(t, it.Remove(a), "removing one replica must keep the route")
	require.Equal(t, 1, it.RouteCount())
	require.Equal(t, 1, it.InstanceCount())

	require.True(t, it.Remove(b), "removing the last replica removes the route")
	require.Equal(t, 0, it.RouteCount())
}

func TestResolveExactAndWildcardPrecedence(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "*", Method: "*", PublicKey: "Xwild", InstanceID: "wild"}))
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "*", PublicKey: "Xpath", InstanceID: "path"}))
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "*", Method: "GET", PublicKey: "Xmethod", InstanceID: "method"}))
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", PublicKey: "Xexact", InstanceID: "exact"}))

	require.Equal(t, Resolution{Found: true, PublicKey: "Xexact", Instances: 1}, it.Resolve("example.com", "/api", "GET"))
	require.Equal(t, Resolution{Found: true, PublicKey: "Xpath", Instances: 1}, it.Resolve("example.com", "/api", "POST"))
	require.Equal(t, Resolution{Found: true, PublicKey: "Xmethod", Instances: 1}, it.Resolve("example.com", "/other", "GET"))
	require.Equal(t, Resolution{Found: true, PublicKey: "Xwild", Instances: 1}, it.Resolve("example.com", "/other", "POST"))
	require.Equal(t, Resolution{}, it.Resolve("other.com", "/api", "GET"))
}

func TestResolveConflictIsScopedToWinningTier(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", PublicKey: "Xexact", InstanceID: "exact"}))
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "*", Method: "*", PublicKey: "Xone", InstanceID: "w1"}))
	require.False(t, it.Add(&Interest{Host: "example.com", Path: "*", Method: "*", PublicKey: "Xtwo", InstanceID: "w2"}))

	// The exact tier wins and is healthy even though a broader wildcard is conflicted.
	require.Equal(t, Resolution{Found: true, PublicKey: "Xexact", Instances: 1}, it.Resolve("example.com", "/api", "GET"))

	// A request that only matches the conflicted wildcard fails closed.
	require.Equal(t, Resolution{Found: true, Conflict: true, Instances: 2}, it.Resolve("example.com", "/other", "POST"))
}

func TestResolveSameKeyAcrossInstancesIsHealthy(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", PublicKey: "Xshared", InstanceID: "a"}))
	require.False(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", PublicKey: "Xshared", InstanceID: "b"}))

	require.Equal(t, Resolution{Found: true, PublicKey: "Xshared", Instances: 2}, it.Resolve("example.com", "/api", "GET"))
}

func TestResolveEmptyAndNonEmptyIsConflict(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", InstanceID: "plain"}))
	require.False(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", PublicKey: "Xkey", InstanceID: "sealed"}))

	require.Equal(t, Resolution{Found: true, Conflict: true, Instances: 2}, it.Resolve("example.com", "/api", "GET"))
}

func TestResolveAllPlaintext(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", InstanceID: "a"}))
	require.False(t, it.Add(&Interest{Host: "example.com", Path: "/api", Method: "GET", InstanceID: "b"}))

	require.Equal(t, Resolution{Found: true, Instances: 2}, it.Resolve("example.com", "/api", "GET"))
}

func TestResolveHostWithPortAndPathWithColon(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{Host: "example.com:8080", Path: "/a:b", Method: "GET", PublicKey: "Xkey", InstanceID: "i1"}))

	require.Equal(t, Resolution{Found: true, PublicKey: "Xkey", Instances: 1}, it.Resolve("example.com:8080", "/a:b", "GET"))
	require.False(t, it.Resolve("example.com", "/a:b", "GET").Found)
}

func TestInterestTrackerWithCustomInterestSubject(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	subject := "test.interests"
	tracker := NewInterestTracker(nc, WithInterestSubject(subject))
	require.NoError(t, tracker.Run())

	interest := Interest{Host: "localhost", Path: "/api", Method: "GET", InstanceID: "i1"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)

	require.NoError(t, nc.Publish(subject+".register.localhost", data))
	require.NoError(t, nc.FlushTimeout(time.Second))

	require.Eventually(t, func() bool {
		return tracker.ResolveRoute("localhost", "/api", "GET").Found
	}, time.Second, 10*time.Millisecond)
}

func TestInterestTrackerAcceptsLegacyPayloadWithoutInstanceID(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	subject := "test.interests.legacy"
	tracker := NewInterestTracker(nc, WithInterestSubject(subject))
	require.NoError(t, tracker.Run())

	// Exact pre-crypto payload: no "pub" and no "instance".
	payload := []byte(`{"host":"legacy.local","path":"/api","method":"GET"}`)
	require.NoError(t, nc.Publish(subject+".register.local.legacy", payload))
	require.NoError(t, nc.FlushTimeout(time.Second))

	require.Eventually(t, func() bool {
		return tracker.ResolveRoute("legacy.local", "/api", "GET").Found
	}, time.Second, 10*time.Millisecond)
	resolution := tracker.ResolveRoute("legacy.local", "/api", "GET")
	require.Empty(t, resolution.PublicKey)
	require.False(t, resolution.Conflict)
	require.Equal(t, 1, resolution.Instances)

	require.NoError(t, nc.Publish(subject+".unregister.local.legacy", payload))
	require.Eventually(t, func() bool {
		return !tracker.ResolveRoute("legacy.local", "/api", "GET").Found
	}, time.Second, 10*time.Millisecond)
}

func TestInterestTrackerWithHostFilters(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	baseSubject := "h8s.control.interest"
	tracker := NewInterestTracker(nc,
		WithInterestSubject(baseSubject),
		WithHostFilters([]string{"example.com"}),
	)
	require.NoError(t, tracker.Run())

	interest := Interest{Host: "example.com", Path: "/api", Method: "GET", InstanceID: "i1"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(baseSubject+".register.com.example", data))

	other := Interest{Host: "other.com", Path: "/api", Method: "GET", InstanceID: "i2"}
	otherData, err := json.Marshal(other)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(baseSubject+".register.com.other", otherData))
	require.NoError(t, nc.FlushTimeout(time.Second))

	require.Eventually(t, func() bool {
		return tracker.ResolveRoute("example.com", "/api", "GET").Found
	}, time.Second, 10*time.Millisecond)
	require.False(t, tracker.ResolveRoute("other.com", "/api", "GET").Found)
}

func TestInterestTrackerUnregisterViaSubject(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	baseSubject := "h8s.control.interest"
	tracker := NewInterestTracker(nc,
		WithInterestSubject(baseSubject),
		WithHostFilters([]string{"example.com"}),
	)
	require.NoError(t, tracker.Run())

	interest := Interest{Host: "example.com", Path: "/api", Method: "GET", InstanceID: "i1"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)

	require.NoError(t, nc.Publish(baseSubject+".register.com.example", data))
	require.Eventually(t, func() bool {
		return tracker.ResolveRoute("example.com", "/api", "GET").Found
	}, time.Second, 10*time.Millisecond)

	// A different instance cannot remove the route.
	otherInstance := interest
	otherInstance.InstanceID = "i2"
	otherData, err := json.Marshal(otherInstance)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(baseSubject+".unregister.com.example", otherData))
	require.NoError(t, nc.FlushTimeout(time.Second))
	require.True(t, tracker.ResolveRoute("example.com", "/api", "GET").Found)

	require.NoError(t, nc.Publish(baseSubject+".unregister.com.example", data))
	require.Eventually(t, func() bool {
		return !tracker.ResolveRoute("example.com", "/api", "GET").Found
	}, time.Second, 10*time.Millisecond)
}

func TestInterestsAddReturnValueAndRouteGauge(t *testing.T) {
	it := newTestInterests()
	interest := &Interest{Host: "localhost", Path: "/ws", Method: "GET", InstanceID: "i1"}
	require.True(t, it.Add(interest))
	require.False(t, it.Add(interest))
	require.Equal(t, 1, it.RouteCount())
}

func TestInterestTrackerResolveWebSocketUsesWSMethod(t *testing.T) {
	it := newTestInterests()
	require.True(t, it.Add(&Interest{
		Host: "example.com", Path: "/socket", Method: "ws",
		PublicKey: "Xkey", InstanceID: "ws-1",
	}))

	tracker := &InterestTracker{Interests: it}
	wsReq := http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/socket"},
		Header: http.Header{"Upgrade": []string{"websocket"}},
	}
	require.Equal(t, Resolution{Found: true, PublicKey: "Xkey", Instances: 1}, tracker.Resolve(wsReq))

	plainReq := wsReq
	plainReq.Header = nil
	require.False(t, tracker.Resolve(plainReq).Found, "a plain GET must not match the ws interest")
}
