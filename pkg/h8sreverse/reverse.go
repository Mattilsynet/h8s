package h8sreverse

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/nats-io/nats.go"
)

// ReverseProxy forwards NATS requests to HTTP and publishes back.
// It can subscribe to a filtered set of subjects based on the subjectmapper rules.

type ReverseProxy struct {
	nats   *nats.Conn
	client *http.Client
}

// NewReverseProxy creates a proxy with a default HTTP client.
func NewReverseProxy(nc *nats.Conn) *ReverseProxy {
	return &ReverseProxy{
		nats:   nc,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// SubscribeOptions holds optional filter settings.
// If Methods is empty, all HTTP methods are accepted.
// If Paths is empty, all paths are accepted.
// If Host is empty, all hosts are accepted.
// Subscriptions are constructed using subjectmapper.SubjectPrefix and wildcards.
// Example: Methods: []string{"GET", "POST"} => subject "h8s.http.GET.*".
// Paths: []string{"/api/users"} => subject "h8s.http.GET.h8s/..api/users".
// This function builds the wildcard subjects accordingly.
func (r *ReverseProxy) SubscribeAll(ctx context.Context) error {
	patterns := []string{
		subjectmapper.SubjectPrefix + ".http.*.*.>",
		subjectmapper.SubjectPrefix + ".ws.ws.*.>",
	}
	for _, pat := range patterns {
		_, err := r.nats.Subscribe(pat, r.handleMsg)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ReverseProxy) handleMsg(msg *nats.Msg) {
	parts := strings.Split(msg.Subject, ".")
	if len(parts) < 5 {
		slog.Error("invalid subject", "subject", msg.Subject)
		return
	}
	scheme := parts[1]
	method := parts[2]
	host := parts[3]
	path := strings.Join(parts[4:], "/")
	urlStr := scheme + "://" + host + "/" + path
	req, err := http.NewRequest(method, urlStr, bytes.NewReader(msg.Data))
	if err != nil {
		slog.Error("new request", "error", err)
		return
	}
	for k, v := range msg.Header {
		for _, vv := range v {
			req.Header.Add(k, vv)
		}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		slog.Error("http error", "error", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	r.nats.Publish(msg.Reply, body)
}
