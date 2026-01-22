package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/Mattilsynet/h8s/pkg/h8sreverse"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type Controller struct {
	client kubernetes.Interface
	proxy  *h8sreverse.ReverseProxy

	mu     sync.RWMutex
	routes map[string]map[string]string // map[host]map[path]targetURL

	// knownHosts tracks hosts we have currently subscribed to in NATS.
	// Used to diff against desired state during reconciliation.
	knownHosts map[string]bool

	// cacheSynced is used to prevent reconciliation before the cache is warmed up
	cacheSynced cache.InformerSynced
	// lister allows us to list all Ingresses from the local cache
	lister func() ([]*networkingv1.Ingress, error)
}

func NewController(client kubernetes.Interface, proxy *h8sreverse.ReverseProxy) *Controller {
	return &Controller{
		client:     client,
		proxy:      proxy,
		routes:     make(map[string]map[string]string),
		knownHosts: make(map[string]bool),
	}
}

// Resolve implements h8sreverse.BackendResolver
func (c *Controller) Resolve(ctx context.Context, host, path string) (*url.URL, error) {
	// First, check for exact host match
	if target, ok := c.findMatch(host, path); ok {
		return target, nil
	}

	// If no exact match, we might want to support wildcard hosts if Ingress spec has them (e.g. *.example.com)
	// But h8s subject routing usually delivers specific hosts.
	// For now, simple exact host lookup.

	return nil, nil // Return nil, nil if no backend found (ReverseProxy handles 404/fallback)
}

func (c *Controller) findMatch(host, path string) (*url.URL, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	paths, ok := c.routes[host]
	if !ok {
		return nil, false
	}

	// Longest prefix match
	var bestMatch string
	var target string

	// Iterate over all registered paths for this host
	for prefix, t := range paths {
		// Ingress paths often start with /, but NATS subject paths might not have leading /.
		// Ensure consistent comparison.
		// h8sreverse/reverse.go constructs path from subject parts, usually without leading slash unless added.
		// Let's assume input path starts with / for standard HTTP comparison.

		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		// Check if request path matches the ingress path prefix
		if strings.HasPrefix(path, prefix) {
			if len(prefix) > len(bestMatch) {
				bestMatch = prefix
				target = t
			}
		}
	}

	if target == "" {
		return nil, false
	}

	u, err := url.Parse(target)
	if err != nil {
		slog.Error("Failed to parse target URL from route", "target", target, "error", err)
		return nil, false
	}
	return u, true
}

func (c *Controller) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(c.client, 0)
	ingressInformer := factory.Networking().V1().Ingresses().Informer()
	ingressLister := factory.Networking().V1().Ingresses().Lister()

	c.cacheSynced = ingressInformer.HasSynced
	c.lister = func() ([]*networkingv1.Ingress, error) {
		return ingressLister.List(labels.Everything())
	}

	ingressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	})

	slog.Info("Starting Ingress Controller")
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	c.cacheSynced = func() bool { return true } // Mark as synced manually if needed or just rely on WaitForCacheSync
	slog.Info("Ingress Controller Synced")
	return nil
}

func (c *Controller) onAdd(obj interface{}) {
	c.reconcile()
}

func (c *Controller) onUpdate(old, new interface{}) {
	c.reconcile()
}

func (c *Controller) onDelete(obj interface{}) {
	c.reconcile()
}

// reconcile rebuilds the routing table from the current state of the Informer cache.
// It ensures that:
// 1. The in-memory routing table matches the Ingress resources.
// 2. NATS subscriptions are added for new hosts.
// 3. NATS subscriptions are removed for deleted hosts.
func (c *Controller) reconcile() {
	// 1. List all Ingresses from cache
	ingresses, err := c.lister()
	if err != nil {
		slog.Error("Failed to list ingresses", "error", err)
		return
	}

	// 2. Build desired routing table
	desiredRoutes := make(map[string]map[string]string)
	desiredHosts := make(map[string]bool)

	for _, ing := range ingresses {
		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				continue
			}

			if _, ok := desiredRoutes[host]; !ok {
				desiredRoutes[host] = make(map[string]string)
			}
			desiredHosts[host] = true

			if rule.HTTP == nil {
				continue
			}

			for _, path := range rule.HTTP.Paths {
				svcName := path.Backend.Service.Name
				svcPort := int32(80) // default
				if path.Backend.Service.Port.Number > 0 {
					svcPort = path.Backend.Service.Port.Number
				}
				ns := ing.Namespace

				// Construct K8s DNS
				targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svcName, ns, svcPort)

				// Normalize path
				p := path.Path
				if p == "" {
					p = "/"
				}
				desiredRoutes[host][p] = targetURL
			}
		}
	}

	// 3. Update state and manage subscriptions
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update routing table
	c.routes = desiredRoutes

	// Identify hosts to unsubscribe (in knownHosts but not in desiredHosts)
	for host := range c.knownHosts {
		if !desiredHosts[host] {
			slog.Info("Removing subscription for host", "host", host)
			if err := c.proxy.UnsubscribeForHost(host); err != nil {
				slog.Error("Failed to unsubscribe host", "host", host, "error", err)
			}
			delete(c.knownHosts, host)
		}
	}

	// Identify hosts to subscribe (in desiredHosts but not in knownHosts)
	for host := range desiredHosts {
		if !c.knownHosts[host] {
			slog.Info("Adding subscription for host", "host", host)
			// We spawn this because it might block or involve network I/O
			// Note: Inside reconcile loop, spawning go routines for subscription might lead to races
			// if reconcile is called rapidly. ideally SubscribeForHost should be fast or idempotent.
			// Given NATS subscribe is fast, we can do it synchronously to ensure state consistency.
			// Or if we must async, we need to be careful. Let's try synchronous first for safety.
			if err := c.proxy.SubscribeForHost(context.Background(), host); err != nil {
				slog.Error("Failed to subscribe for host", "host", host, "error", err)
				// Don't mark as known if failed, so we retry next reconcile
			} else {
				c.knownHosts[host] = true
			}
		}
	}

	slog.Info("Reconciliation complete", "hosts", len(c.knownHosts), "routes", len(c.routes))
}

// Deprecated: updateIngress is replaced by reconcile
func (c *Controller) updateIngress(ing *networkingv1.Ingress) {
	// No-op, kept for reference if needed during refactor, but can be deleted.
}
