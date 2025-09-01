package tracker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/internal/natstest"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestInterestId(t *testing.T) {
	tests := []struct {
		name     string
		interest Interest
		want     string
	}{
		{
			"simple",
			Interest{"host1", "/path", "GET"},
			"host1:/path:GET",
		},
		{"trailing slash", Interest{"host2", "/", "GET"}, "host2:/:GET"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.interest.Id())
		})
	}
}

func TestInterestsAddAndEvict(t *testing.T) {
	it := &Interests{
		InterestMap:  make(map[string]*Interest),
		InterestSeen: make(map[string]time.Time),
	}

	interest := &Interest{Host: "localhost", Path: "/ws"}
	it.Add(interest)

	id := interest.Id()
	require.Contains(t, it.InterestMap, id)
	require.Contains(t, it.InterestSeen, id)

	it.EvictStale(time.Now().Add(3 * time.Minute))
	require.NotContains(t, it.InterestMap, id)
	require.NotContains(t, it.InterestSeen, id)
}

func TestEvictStale(t *testing.T) {
	it := &Interests{
		InterestMap:  map[string]*Interest{},
		InterestSeen: map[string]time.Time{},
	}
	interest := &Interest{Host: "foo", Path: "/bar"}
	id := interest.Id()
	it.InterestMap[id] = interest
	it.InterestSeen[id] = time.Now().Add(-3 * time.Minute)

	it.EvictStale(time.Now())

	require.NotContains(t, it.InterestMap, id)
	require.NotContains(t, it.InterestSeen, id)
}

func TestInterestTrackerWithCustomInterestSubject(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	// nc, err := nats.Connect("nats://localhost:4222")
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	subject := "test.interests"
	tracker := NewInterestTracker(nc, WithInterestSubject(subject))
	err = tracker.Run()
	require.NoError(t, err)

	interest := Interest{Host: "localhost", Path: "/api"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)

	err = nc.Publish(subject, data)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	id := interest.Id()
	tracker.Interests.RLock()
	defer tracker.Interests.RUnlock()
	require.Contains(t, tracker.Interests.InterestMap, id)
}

// This helper added for testability
func (it *Interests) EvictStale(now time.Time) {
	it.Lock()
	defer it.Unlock()
	for id, ts := range it.InterestSeen {
		if now.Sub(ts) > 2*time.Minute {
			delete(it.InterestMap, id)
			delete(it.InterestSeen, id)
		}
	}
}
