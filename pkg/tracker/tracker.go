package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// InterestTracker tracks hosts and their discovered WebSocket and request/reply connection handlers.
type InterestTracker struct {
	nc                *nats.Conn
	interestSubject   string
	hostFilters       []string // When set, subscribe to per-host interest subjects instead of the global one
	Interests         *Interests
	OTELMeter         metric.Meter
	NumberOfInterests metric.Int64Gauge
}

type Interests struct {
	sync.RWMutex
	InterestMap  map[string]*Interest
	InterestSeen map[string]time.Time
}

type Interest struct {
	Host   string `json:"host"`
	Path   string `json:"path"`
	Method string `json:"method"`
}

func (i *Interest) Id() string {
	return fmt.Sprintf("%s:%s:%s", i.Host, i.Path, i.Method)
}

type InterestTrackerOption func(*InterestTracker)

func NewInterestTracker(nc *nats.Conn, opts ...InterestTrackerOption) *InterestTracker {
	tracker := &InterestTracker{
		nc: nc,
		Interests: &Interests{
			InterestMap:  make(map[string]*Interest),
			InterestSeen: make(map[string]time.Time),
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
	// Runs eviction of stale interests as a goroutine
	go it.Interests.RunEvictions()

	registerHandler := func(msg *nats.Msg) {
		interest := &Interest{}
		if err := json.Unmarshal(msg.Data, interest); err != nil {
			slog.Error("error unmarshalling interest payload", "error", err)
			return
		}
		changed := it.Interests.Add(interest)
		// Update gauge when number of registered Interests change.
		if changed {
			it.Interests.RLock()
			count := int64(len(it.Interests.InterestMap))
			it.Interests.RUnlock()
			it.NumberOfInterests.Record(context.Background(), count)
		}
	}

	unregisterHandler := func(msg *nats.Msg) {
		interest := &Interest{}
		if err := json.Unmarshal(msg.Data, interest); err != nil {
			slog.Error("error unmarshalling interest unregister payload", "error", err)
			return
		}
		removed := it.Interests.Remove(interest)
		if removed {
			it.Interests.RLock()
			count := int64(len(it.Interests.InterestMap))
			it.Interests.RUnlock()
			it.NumberOfInterests.Record(context.Background(), count)
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

func (it *InterestTracker) ValidRequest(req http.Request) bool {
	it.Interests.RLock()
	defer it.Interests.RUnlock()

	tempInterest := &Interest{
		req.Host,
		req.URL.Path,
		req.Method,
	}
	if interest, exists := it.Interests.InterestMap[tempInterest.Id()]; exists && interest != nil {
		return true
	}
	slog.Debug("no registered interest for request", "interest", tempInterest)
	return false
}

// Add registers or refreshes an interest. Returns true if this is a newly
// added interest (not just a timestamp refresh), enabling callers to update
// gauges only when the map size actually changes.
func (i *Interests) Add(interest *Interest) bool {
	id := interest.Id()
	i.Lock()
	_, existed := i.InterestMap[id]
	i.InterestMap[id] = interest
	i.InterestSeen[id] = time.Now()
	i.Unlock()
	if !existed {
		slog.Info("Added new interest", "interest", interest)
	}
	return !existed
}

func (i *Interests) RunEvictions() {
	for {
		i.RLock()
		slog.Debug("Current interests", "interests", i.InterestMap)
		i.RUnlock()
		time.Sleep(5 * time.Second) // Initial delay before starting evictions

		now := time.Now()
		i.Lock()
		for id, ts := range i.InterestSeen {
			if now.Sub(ts) > 1*time.Minute {
				delete(i.InterestMap, id)
				delete(i.InterestSeen, id)
			}
		}
		i.Unlock()
	}
}

func (i *Interests) evict(id string) {
	i.Lock()
	delete(i.InterestMap, id)
	delete(i.InterestSeen, id)
	i.Unlock()
}

// Remove removes an interest by its Interest value. Returns true if the
// interest was present and removed.
func (i *Interests) Remove(interest *Interest) bool {
	id := interest.Id()
	i.Lock()
	_, existed := i.InterestMap[id]
	if existed {
		delete(i.InterestMap, id)
		delete(i.InterestSeen, id)
	}
	i.Unlock()
	if existed {
		slog.Info("Removed interest", "interest", interest)
	}
	return existed
}
