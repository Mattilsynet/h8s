package ingress

import (
	"context"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/pkg/h8sreverse"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestControllerLifecycle(t *testing.T) {
	// Setup Fake K8s Client
	client := fake.NewSimpleClientset()

	// Initialize controller manually to avoid side effects of full run/start
	// and to inject dependencies if needed (though we use struct directly here)
	ctrl := NewController(client, &h8sreverse.ReverseProxy{})

	// Hack: Pre-fill knownHosts for hosts we expect to test, so we skip the SubscribeForHost call
	// which would fail with nil/empty Proxy.
	// This is testing the RECONCILE logic (map updates), not the NATS integration.
	// But wait, Reconcile is triggered by Informer.
	// If we want to test "Ingress Created -> Route Added", we need Reconcile to run.
	// Reconcile calls SubscribeForHost.
	// If SubscribeForHost fails, it logs error but DOES NOT set knownHosts.
	// However, `routes` ARE updated before subscription.
	// So we can verify `routes` map even if subscription fails.

	// 1. Setup Informer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	factory := informers.NewSharedInformerFactory(client, 0)
	ingressInformer := factory.Networking().V1().Ingresses().Informer()
	ingressLister := factory.Networking().V1().Ingresses().Lister()

	// Hook up controller to this informer manually (simulating Run())
	ctrl.lister = func() ([]*networkingv1.Ingress, error) {
		// We can't easily use lister here because it relies on cache sync.
		// For unit test, we can mock lister or use the real one and wait for sync.
		return ingressLister.List(labels.Everything())
	}

	ingressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.onAdd,
		UpdateFunc: ctrl.onUpdate,
		DeleteFunc: ctrl.onDelete,
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// 2. Add an Ingress
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lifecycle-test",
			Namespace: "default",
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: "lifecycle.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path: "/",
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "lifecycle-svc",
											Port: networkingv1.ServiceBackendPort{Number: 80},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	client.NetworkingV1().Ingresses("default").Create(ctx, ing, metav1.CreateOptions{})

	// Allow some time for informer to trigger
	time.Sleep(100 * time.Millisecond)

	// 3. Verify Route Added
	// The route should be present in the map
	url, _ := ctrl.Resolve(ctx, "lifecycle.example.com", "/")
	require.NotNil(t, url, "Route should be found after Ingress creation")
	require.Equal(t, "http://lifecycle-svc.default.svc.cluster.local:80", url.String())

	// 4. Delete Ingress
	client.NetworkingV1().Ingresses("default").Delete(ctx, "lifecycle-test", metav1.DeleteOptions{})

	// Allow some time for informer to trigger
	time.Sleep(100 * time.Millisecond)

	// 5. Verify Route Removed
	url, _ = ctrl.Resolve(ctx, "lifecycle.example.com", "/")
	require.Nil(t, url, "Route should be removed after Ingress deletion")
}
