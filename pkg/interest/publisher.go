// Package interest publishes and unregisters responder interests.
package interest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/nats-io/nats.go"
)

// DefaultInterval is frequent enough for the tracker's one-minute TTL while
// keeping interest traffic small.
const DefaultInterval = 5 * time.Second

// Publisher periodically publishes the current set of interests to
// <Subject>.register.<reversed-host>. Unregister publishes the same set to
// <Subject>.unregister.<reversed-host>.
type Publisher struct {
	NATSConn *nats.Conn
	Subject  string
	Interval time.Duration
	// Entries must return a snapshot; Publisher does not own the returned
	// slice and may call it from a background goroutine.
	Entries func() []tracker.Interest
}

// Run publishes immediately, then every Interval until ctx is canceled.
func (p *Publisher) Run(ctx context.Context) {
	if p == nil || p.NATSConn == nil || p.Entries == nil {
		return
	}
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	p.publish("register")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.publish("register")
		}
	}
}

// Unregister publishes a best-effort unregister for every current entry and
// flushes briefly so the messages leave before the NATS connection closes.
func (p *Publisher) Unregister() {
	if p == nil || p.NATSConn == nil || p.Entries == nil {
		return
	}
	p.publish("unregister")
	_ = p.NATSConn.FlushTimeout(500 * time.Millisecond)
}

func (p *Publisher) publish(action string) {
	for _, entry := range p.Entries() {
		if entry.Host == "" {
			continue
		}
		data, err := json.Marshal(entry)
		if err != nil {
			slog.Error("unable to marshal interest", "error", err, "action", action, "interest", entry)
			continue
		}
		subject := p.Subject + "." + action + "." + subjectmapper.ReverseHostname(entry.Host)
		if err := p.NATSConn.Publish(subject, data); err != nil {
			slog.Error("unable to publish interest", "error", err, "subject", subject)
		}
	}
}
