package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// InterestTracker tracks hosts and their discovered WebSocket and request/reply connection handlers.
type InterestTracker struct {
	nc                *nats.Conn
	interestSubject   string
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

func WithOTELMeter(meter metric.Meter) InterestTrackerOption {
	return func(it *InterestTracker) {
		it.OTELMeter = meter
	}
}

func (it *InterestTracker) Run() error {
	// Runs eviction of stale interests as a goroutine
	go it.Interests.RunEvictions()

	// Track number of endpoints registered as Interests.
	endpoints := int64(len(it.Interests.InterestMap))

	if _, err := it.nc.Subscribe(
		it.interestSubject,
		func(msg *nats.Msg) {
			interest := &Interest{}
			if err := json.Unmarshal(msg.Data, interest); err != nil {
				slog.Error("error unmarshalling interest payload", "error", err)
			}
			it.Interests.Add(interest)
			// Update gauge when number of registered Interests change.
			if int64(len(it.Interests.InterestMap)) != endpoints {
				it.NumberOfInterests.Record(
					context.Background(),
					int64(len(it.Interests.InterestMap)),
				)
			}
		}); err != nil {
		return err
	}
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

func (i *Interests) Add(interest *Interest) {
	slog.Info("Adding interest", "interest", interest)
	i.Lock()
	i.InterestMap[interest.Id()] = interest
	i.InterestSeen[interest.Id()] = time.Now()
	i.Unlock()
}

func (i *Interests) RunEvictions() {
	for {
		slog.Debug("Current interests", "interests", i.InterestMap)
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
