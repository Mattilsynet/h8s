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
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type Controller struct {
	client     kubernetes.Interface
	proxy      *h8sreverse.ReverseProxy
	routes     sync.Map // map[host]map[path]targetURL
	knownHosts sync.Map // map[host]bool
}

func NewController(client kubernetes.Interface, proxy *h8sreverse.ReverseProxy) *Controller {
	return &Controller{
		client: client,
		proxy:  proxy,
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
	pathsRaw, ok := c.routes.Load(host)
	if !ok {
		return nil, false
	}
	paths := pathsRaw.(map[string]string)

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

	ingressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	})

	slog.Info("Starting Ingress Controller")
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	slog.Info("Ingress Controller Synced")
	return nil
}

func (c *Controller) onAdd(obj interface{}) {
	ing := obj.(*networkingv1.Ingress)
	c.updateIngress(ing)
}

func (c *Controller) onUpdate(old, new interface{}) {
	ing := new.(*networkingv1.Ingress)
	c.updateIngress(ing)
}

func (c *Controller) onDelete(obj interface{}) {
	ing := obj.(*networkingv1.Ingress)
	slog.Info("Ingress deleted", "namespace", ing.Namespace, "name", ing.Name)

	// A robust implementation would selectively remove rules.
	// For this MVP, we are adding/updating aggressively.
	// Since we store routes by Host, deleting an ingress means we should re-calculate routes for the affected hosts.
	// But if multiple ingresses share a host, naive deletion is risky.
	// Ideally we'd rebuild the entire routing table from the informer cache.
	// For now, we will just log. Real updates happen via Update.
}

func (c *Controller) updateIngress(ing *networkingv1.Ingress) {
	for _, rule := range ing.Spec.Rules {
		host := rule.Host
		if host == "" {
			continue
		}

		// Load existing map or create new
		// note: this is a simple merge strategy. It doesn't handle conflicts or deletions well.
		// For a robust controller, we should rebuild the map for the host from ALL ingresses.

		pathMap := make(map[string]string)
		if existing, ok := c.routes.Load(host); ok {
			for k, v := range existing.(map[string]string) {
				pathMap[k] = v
			}
		}

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
			pathMap[p] = targetURL
			slog.Info("Registered route", "host", host, "path", p, "target", targetURL)
		}

		c.routes.Store(host, pathMap)

		// Subscribe to NATS if not already done
		if _, exists := c.knownHosts.Load(host); !exists {
			slog.Info("New Ingress Host found, subscribing", "host", host)
			// We spawn this because it might block or involve network I/O
			go func(h string) {
				err := c.proxy.SubscribeForHost(context.Background(), h)
				if err != nil {
					slog.Error("Failed to subscribe for host", "host", h, "error", err)
				} else {
					c.knownHosts.Store(h, true)
				}
			}(host)
		}
	}
}
