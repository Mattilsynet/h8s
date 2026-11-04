package h8sreverse

import (
	"context"
	"net/url"
)

// BackendResolver defines how to find the target backend for a request.
// Resolve may be called concurrently. Returned URLs must remain read-only;
// the proxy copies them before adjusting request paths.
type BackendResolver interface {
	// Resolve returns the target backend URL for a given host and path.
	Resolve(ctx context.Context, host, path string) (*url.URL, error)
}

// StaticResolver adapts the old static flags to the new interface (for h8srd).
type StaticResolver struct {
	BackendURL *url.URL
}

func (s *StaticResolver) Resolve(ctx context.Context, host, path string) (*url.URL, error) {
	return s.BackendURL, nil
}
