package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const (
	// interestTTL is deliberately much larger than the 5s publish interval so
	// a single missed publish does not evict a live instance.
	interestTTL = time.Minute

	// legacyInstanceID is assigned to registrations from publishers that
	// predate the InstanceID field. All legacy replicas of one route collapse
	// into this single entry, which matches the old route-level semantics.
	// The value contains characters outside the NUID alphabet to avoid a
	// collision with a real generated instance ID.
	legacyInstanceID = "<legacy>"
)

// InterestTracker tracks hosts and their discovered WebSocket and
// request/reply connection handlers.
type InterestTracker struct {
	nc                *nats.Conn
	interestSubject   string
	hostFilters       []string // When set, subscribe to per-host interest subjects instead of the global one
	Interests         *Interests
	OTELMeter         metric.Meter
	NumberOfInterests metric.Int64Gauge
}

// routeKey identifies a route. Struct fields avoid the ambiguity of a
// concatenated host:path:method key (ports, IPv6 literals, and paths
// containing ':' are all valid).
type routeKey struct {
	Host   string
	Path   string
	Method string
}

type instanceInterest struct {
	interest Interest
	seen     time.Time
}

// Interests stores a route as a set of live instances. Multiple replicas of
// the same service may advertise the same route; each must be added and
// removed independently.
type Interests struct {
	sync.RWMutex
	routes map[routeKey]map[string]*instanceInterest
}

// Interest is the wire representation advertised by a responder.
type Interest struct {
	Host       string `json:"host"`
	Path       string `json:"path"`
	Method     string `json:"method"`
	PublicKey  string `json:"pub,omitempty"`
	InstanceID string `json:"instance"`
}

// Id returns the route identity. It is retained for logs and tests; internal
// matching uses routeKey so delimiters in host/path cannot collide.
func (i *Interest) Id() string {
	return fmt.Sprintf("%s:%s:%s", i.Host, i.Path, i.Method)
}

// Resolution is the result of resolving an HTTP request against the interest
// table. Conflict means the winning route has more than one distinct non-empty
// public key, or a mix of empty and non-empty keys. Callers must fail closed
// on conflict rather than falling back to plaintext.
type Resolution struct {
	Found     bool
	PublicKey string
	Conflict  bool
	Instances int
}

type InterestTrackerOption func(*InterestTracker)

func NewInterestTracker(nc *nats.Conn, opts ...InterestTrackerOption) *InterestTracker {
	tracker := &InterestTracker{
		nc: nc,
		Interests: &Interests{
			routes: make(map[routeKey]map[string]*instanceInterest),
		},
		OTELMeter: otel.GetMeterProvider().Meter("interest-tracker"),
	}
	for _, opt := range opts {
		opt(tracker)
	}
	var err error

	tracker.NumberOfInterests, err = tracker.OTELMeter.Int64Gauge(
		"number_of_interests",
		metric.WithDescription("Number of endpoints (interests) registred on this InterestTracker instance."))
	if err != nil {
		slog.Error("failed to create OTEL gauge for number of interests", "error", err)
	}
	return tracker
}

func WithInterestSubject(subject string) InterestTrackerOption {
	return func(it *InterestTracker) {
		it.interestSubject = subject
	}
}

// WithHostFilters configures the tracker to subscribe only to interest
// subjects scoped to the given hostnames. When set, instead of subscribing
// to wildcard subjects, the tracker subscribes to
// <interestSubject>.register.<reversed-host> and
// <interestSubject>.unregister.<reversed-host> for each host.
// This eliminates cross-host mutex contention from interest messages
// for unrelated hostnames.
func WithHostFilters(hosts []string) InterestTrackerOption {
	return func(it *InterestTracker) {
		it.hostFilters = hosts
	}
}

func WithOTELMeter(meter metric.Meter) InterestTrackerOption {
	return func(it *InterestTracker) {
		it.OTELMeter = meter
	}
}

func (it *InterestTracker) Run() error {
	// Runs eviction of stale instances as a goroutine.
	go it.runEvictions()

	registerHandler := func(msg *nats.Msg) {
		interest := &Interest{}
		if err := json.Unmarshal(msg.Data, interest); err != nil {
			slog.Error("error unmarshalling interest payload", "error", err)
			return
		}
		if it.Interests.Add(interest) {
			it.recordRouteCount()
		}
	}

	unregisterHandler := func(msg *nats.Msg) {
		interest := &Interest{}
		if err := json.Unmarshal(msg.Data, interest); err != nil {
			slog.Error("error unmarshalling interest unregister payload", "error", err)
			return
		}
		if it.Interests.Remove(interest) {
			it.recordRouteCount()
		}
	}

	// When host filters are configured, subscribe to per-host interest subjects.
	// This avoids processing interest messages for hosts this instance doesn't serve.
	if len(it.hostFilters) > 0 {
		for _, host := range it.hostFilters {
			reversedHost := subjectmapper.ReverseHostname(host)
			regSubject := it.interestSubject + ".register." + reversedHost
			if _, err := it.nc.Subscribe(regSubject, registerHandler); err != nil {
				return fmt.Errorf("subscribe to host interest %q: %w", regSubject, err)
			}
			unregSubject := it.interestSubject + ".unregister." + reversedHost
			if _, err := it.nc.Subscribe(unregSubject, unregisterHandler); err != nil {
				return fmt.Errorf("subscribe to host interest unregister %q: %w", unregSubject, err)
			}
			slog.Info("InterestTracker subscribed to host-scoped interest subjects",
				"register", regSubject, "unregister", unregSubject, "host", host)
		}
		return nil
	}

	// Fallback: no host filters, subscribe to wildcard interest subjects
	// that match all hosts.
	regSubject := it.interestSubject + ".register.>"
	if _, err := it.nc.Subscribe(regSubject, registerHandler); err != nil {
		return err
	}
	unregSubject := it.interestSubject + ".unregister.>"
	if _, err := it.nc.Subscribe(unregSubject, unregisterHandler); err != nil {
		return err
	}
	slog.Info("InterestTracker subscribed to global interest subjects",
		"register", regSubject, "unregister", unregSubject)
	return nil
}

func (it *InterestTracker) recordRouteCount() {
	if it.NumberOfInterests == nil {
		return
	}
	it.NumberOfInterests.Record(context.Background(), int64(it.Interests.RouteCount()))
}

func (it *InterestTracker) runEvictions() {
	// Initial delay before starting evictions.
	time.Sleep(5 * time.Second)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if it.Interests.EvictStale(time.Now()) > 0 {
			it.recordRouteCount()
		}
	}
}

// Resolve resolves an HTTP request against the winning interest tier. A
// WebSocket upgrade is advertised with method "ws", not the handshake's GET.
func (it *InterestTracker) Resolve(req http.Request) Resolution {
	method := req.Method
	if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		method = subjectmapper.WebSocketMethod
	}
	return it.Interests.Resolve(req.Host, req.URL.Path, method)
}

// ResolveRoute resolves a route against the winning interest tier.
func (it *InterestTracker) ResolveRoute(host, path, method string) Resolution {
	return it.Interests.Resolve(host, path, method)
}

// ValidRequest reports whether the request matches a registered interest.
// It is retained for callers that only need the boolean form.
func (it *InterestTracker) ValidRequest(req http.Request) bool {
	return it.Resolve(req).Found
}

func effectiveInstanceID(interest *Interest) string {
	if interest == nil || interest.InstanceID == "" {
		return legacyInstanceID
	}
	return interest.InstanceID
}

// Add registers or refreshes an instance. It returns true only when a new
// route is created, so callers update route gauges only when the route count
// actually changes. Registrations without an InstanceID use legacyInstanceID
// so pre-crypto publishers can still register as plaintext routes.
func (i *Interests) Add(interest *Interest) bool {
	if interest == nil {
		return false
	}

	key := routeKey{Host: interest.Host, Path: interest.Path, Method: interest.Method}
	copyInterest := *interest
	copyInterest.InstanceID = effectiveInstanceID(interest)
	now := time.Now()

	i.Lock()
	instances, routeExisted := i.routes[key]
	if instances == nil {
		instances = make(map[string]*instanceInterest)
		i.routes[key] = instances
	}
	_, instanceExisted := instances[copyInterest.InstanceID]
	instances[copyInterest.InstanceID] = &instanceInterest{interest: copyInterest, seen: now}
	i.Unlock()

	if !routeExisted {
		slog.Info("Added new interest route", "interest", &copyInterest)
	} else if !instanceExisted {
		slog.Debug("Added interest instance", "interest", &copyInterest)
	}
	return !routeExisted
}

// EvictStale removes instances whose last registration is older than the TTL.
// It returns the number of routes removed, which is what the route gauge
// tracks.
func (i *Interests) EvictStale(now time.Time) int {
	i.Lock()
	defer i.Unlock()
	removedRoutes := 0
	for key, instances := range i.routes {
		for instanceID, entry := range instances {
			if now.Sub(entry.seen) > interestTTL {
				delete(instances, instanceID)
			}
		}
		if len(instances) == 0 {
			delete(i.routes, key)
			removedRoutes++
		}
	}
	return removedRoutes
}

func (i *Interests) RunEvictions() {
	for {
		i.RLock()
		slog.Debug("Current interests", "routes", len(i.routes))
		i.RUnlock()
		time.Sleep(5 * time.Second)

		i.EvictStale(time.Now())
	}
}

// Remove removes one instance. The route is deleted only when its last
// instance is removed. An unregister can never remove another instance's
// registration. A legacy unregister without an InstanceID removes only the
// legacy entry, never a newer per-instance registration.
func (i *Interests) Remove(interest *Interest) bool {
	if interest == nil {
		return false
	}
	instanceID := effectiveInstanceID(interest)
	key := routeKey{Host: interest.Host, Path: interest.Path, Method: interest.Method}

	i.Lock()
	instances, routeExists := i.routes[key]
	if !routeExists {
		i.Unlock()
		return false
	}
	if _, instanceExists := instances[instanceID]; !instanceExists {
		i.Unlock()
		return false
	}
	delete(instances, instanceID)
	routeRemoved := len(instances) == 0
	if routeRemoved {
		delete(i.routes, key)
	}
	i.Unlock()

	if routeRemoved {
		slog.Info("Removed interest route", "interest", interest)
	} else {
		slog.Debug("Removed interest instance", "interest", interest)
	}
	return routeRemoved
}

// Resolve returns the resolution for the first tier that has any instances.
// Ordered ladder, most specific first:
//
//	host/path/method, host/path/*, host/*/method, host/*/*
//
// Conflict is evaluated only inside the winning tier. A broader tier with a
// different key does not conflict with a more specific winner.
func (i *Interests) Resolve(host, path, method string) Resolution {
	candidates := []routeKey{
		{Host: host, Path: path, Method: method},
		{Host: host, Path: path, Method: "*"},
		{Host: host, Path: "*", Method: method},
		{Host: host, Path: "*", Method: "*"},
	}

	seen := make(map[routeKey]struct{}, len(candidates))
	for _, key := range candidates {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		i.RLock()
		instances, exists := i.routes[key]
		if !exists || len(instances) == 0 {
			i.RUnlock()
			continue
		}
		snapshot := make([]Interest, 0, len(instances))
		for _, entry := range instances {
			snapshot = append(snapshot, entry.interest)
		}
		i.RUnlock()

		return resolveSnapshot(snapshot)
	}

	return Resolution{}
}

func resolveSnapshot(instances []Interest) Resolution {
	resolution := Resolution{Found: true, Instances: len(instances)}
	distinctKeys := make(map[string]struct{})
	emptyKeys := 0
	for _, instance := range instances {
		if instance.PublicKey == "" {
			emptyKeys++
			continue
		}
		distinctKeys[instance.PublicKey] = struct{}{}
	}

	if len(distinctKeys) > 1 || (len(distinctKeys) == 1 && emptyKeys > 0) {
		resolution.Conflict = true
		return resolution
	}
	if len(distinctKeys) == 1 {
		for publicKey := range distinctKeys {
			resolution.PublicKey = publicKey
		}
	}
	return resolution
}

// RouteCount returns the number of distinct routes.
func (i *Interests) RouteCount() int {
	i.RLock()
	defer i.RUnlock()
	return len(i.routes)
}

// InstanceCount returns the number of live instance registrations.
func (i *Interests) InstanceCount() int {
	i.RLock()
	defer i.RUnlock()
	count := 0
	for _, instances := range i.routes {
		count += len(instances)
	}
	return count
}
