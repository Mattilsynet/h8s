package tracker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// HostTracker tracks hosts and their discovered WebSocket and request/reply connection handlers.
type InterestTracker struct {
	nc              *nats.Conn
	interestSubject string
	Interests       *Interests
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

func NewInterestTracker(nc *nats.Conn, interestSubject string) *InterestTracker {
	return &InterestTracker{
		nc:              nc,
		interestSubject: interestSubject,
		Interests: &Interests{
			InterestMap:  make(map[string]*Interest),
			InterestSeen: make(map[string]time.Time),
		},
	}
}

func (it *InterestTracker) Run() error {
	// Runs eviction of stale interests as a goroutine
	go it.Interests.RunEvictions()

	if _, err := it.nc.Subscribe(
		it.interestSubject,
		func(msg *nats.Msg) {
			interest := &Interest{}
			if err := json.Unmarshal(msg.Data, interest); err != nil {
				slog.Error("error unmarshalling interest payload", "error", err)
			}
			it.Interests.Add(interest)
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
		slog.Error("untable to find interest for request", "interest", interest, "exists", exists)
		return false
	}
	return true
}

func (it *Interests) Add(interest *Interest) {
	slog.Info("Adding interest", "interest", interest)
	it.Lock()
	it.InterestMap[interest.Id()] = interest
	it.InterestSeen[interest.Id()] = time.Now()
	it.Unlock()
}

func (it *Interests) RunEvictions() {
	for {
		slog.Info("Current interests", "interests", it.InterestMap)
		time.Sleep(5 * time.Second) // Initial delay before starting evictions

		for id, ts := range it.InterestSeen {
			now := time.Now()
			dur := now.Sub(ts)
			if dur > 1*time.Minute {
				it.evict(id)
			}
		}
		slog.Info("Number of registered interests", "number", len(it.InterestMap))
	}
}

func (it *Interests) evict(id string) {
	it.Lock()
	delete(it.InterestMap, id)
	delete(it.InterestSeen, id)
	it.Unlock()
}
