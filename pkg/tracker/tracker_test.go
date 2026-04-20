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

	err = nc.Publish(subject+".register.localhost", data)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	id := interest.Id()
	tracker.Interests.RLock()
	defer tracker.Interests.RUnlock()
	require.Contains(t, tracker.Interests.InterestMap, id)
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
	err = tracker.Run()
	require.NoError(t, err)

	// Publish interest on the host-scoped subject (reversed: com.example)
	interest := Interest{Host: "example.com", Path: "/api", Method: "GET"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)

	err = nc.Publish(baseSubject+".register.com.example", data)
	require.NoError(t, err)

	// Publish interest on a DIFFERENT host's subject — should NOT be received
	other := Interest{Host: "other.com", Path: "/api", Method: "GET"}
	otherData, err := json.Marshal(other)
	require.NoError(t, err)
	err = nc.Publish(baseSubject+".register.com.other", otherData)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	tracker.Interests.RLock()
	defer tracker.Interests.RUnlock()
	require.Contains(t, tracker.Interests.InterestMap, interest.Id(), "should contain interest for example.com")
	require.NotContains(t, tracker.Interests.InterestMap, other.Id(), "should NOT contain interest for other.com")
}

func TestInterestsAddReturnValue(t *testing.T) {
	it := &Interests{
		InterestMap:  make(map[string]*Interest),
		InterestSeen: make(map[string]time.Time),
	}

	interest := &Interest{Host: "localhost", Path: "/ws", Method: "GET"}

	// First add should return true (new)
	require.True(t, it.Add(interest), "first Add should return true")

	// Second add of same interest should return false (refresh)
	require.False(t, it.Add(interest), "duplicate Add should return false")
}

func TestInterestsRemove(t *testing.T) {
	it := &Interests{
		InterestMap:  make(map[string]*Interest),
		InterestSeen: make(map[string]time.Time),
	}

	interest := &Interest{Host: "localhost", Path: "/api", Method: "GET"}
	it.Add(interest)

	// Remove should return true for existing interest
	require.True(t, it.Remove(interest))
	require.NotContains(t, it.InterestMap, interest.Id())
	require.NotContains(t, it.InterestSeen, interest.Id())

	// Remove should return false for non-existent interest
	require.False(t, it.Remove(interest))
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
	err = tracker.Run()
	require.NoError(t, err)

	// Register an interest
	interest := Interest{Host: "example.com", Path: "/api", Method: "GET"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)

	err = nc.Publish(baseSubject+".register.com.example", data)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	tracker.Interests.RLock()
	require.Contains(t, tracker.Interests.InterestMap, interest.Id())
	tracker.Interests.RUnlock()

	// Unregister the interest via the unregister subject
	err = nc.Publish(baseSubject+".unregister.com.example", data)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	tracker.Interests.RLock()
	require.NotContains(t, tracker.Interests.InterestMap, interest.Id(), "interest should be removed after unregister")
	tracker.Interests.RUnlock()
}

func TestInterestTrackerUnregisterGlobalSubject(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()
	defer nc.Close()

	subject := "test.interests"
	tracker := NewInterestTracker(nc, WithInterestSubject(subject))
	err = tracker.Run()
	require.NoError(t, err)

	// Register an interest
	interest := Interest{Host: "localhost", Path: "/api", Method: "GET"}
	data, err := json.Marshal(interest)
	require.NoError(t, err)

	err = nc.Publish(subject+".register.localhost", data)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	tracker.Interests.RLock()
	require.Contains(t, tracker.Interests.InterestMap, interest.Id())
	tracker.Interests.RUnlock()

	// Unregister via global unregister subject
	err = nc.Publish(subject+".unregister.localhost", data)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	tracker.Interests.RLock()
	require.NotContains(t, tracker.Interests.InterestMap, interest.Id(), "interest should be removed after unregister")
	tracker.Interests.RUnlock()
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
