package ingress

import (
	"context"
	"testing"

	"github.com/Mattilsynet/h8s/pkg/h8sreverse"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// mockProxy wraps ReverseProxy to allow mocking SubscribeForHost if we needed to verify calls.
// Since SubscribeForHost is a concrete method on *ReverseProxy, mocking it requires interface extraction
// or just ignoring the side effect (NATS subscription) which is what we do here by passing nil or a real proxy with mocked NATS.
// For this unit test, we focus on the routing logic in Controller.

func TestController_Routing(t *testing.T) {
	// 1. Setup
	client := fake.NewSimpleClientset()
	// We pass nil proxy because we are testing the Resolve logic, not the side-effect of subscription
	// The code checks if proxy is nil before calling? No, it calls c.proxy.SubscribeForHost.
	// To avoid panic, we need a proxy instance.
	// NewReverseProxy requires a NATS conn.
	// Let's modify the controller to accept an interface or just handle nil proxy gracefully for tests,
	// OR better: Create a proxy with a nil NATS conn? NewReverseProxy might panic.
	// Let's create a proxy with a dummy NATS conn if needed, but actually
	// we can just test updateIngress logic carefully.
	// Alternatively, we can construct the controller struct directly since it's in the same package (whitebox).

	ctrl := &Controller{
		client: client,
		// proxy: nil, // We must ensure we don't hit the subscription logic that uses proxy
	}

	// Note: knownHosts check in updateIngress prevents calling SubscribeForHost if we pre-populate it.
	ctrl.knownHosts.Store("api.example.com", true)
	ctrl.knownHosts.Store("app.example.com", true)

	// 2. Define an Ingress
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: "api.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/v1",
									PathType: func() *networkingv1.PathType { p := networkingv1.PathTypePrefix; return &p }(),
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "api-service",
											Port: networkingv1.ServiceBackendPort{Number: 8080},
										},
									},
								},
							},
						},
					},
				},
				{
					Host: "app.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: func() *networkingv1.PathType { p := networkingv1.PathTypePrefix; return &p }(),
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "frontend",
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

	// 3. Trigger Update
	ctrl.updateIngress(ing)

	// 4. Verify Resolution

	tests := []struct {
		name      string
		host      string
		path      string
		wantURL   string
		wantFound bool
	}{
		{
			name:      "API Match",
			host:      "api.example.com",
			path:      "/v1/users",
			wantURL:   "http://api-service.default.svc.cluster.local:8080",
			wantFound: true,
		},
		{
			name:      "API Match Exact",
			host:      "api.example.com",
			path:      "/v1",
			wantURL:   "http://api-service.default.svc.cluster.local:8080",
			wantFound: true,
		},
		{
			name:      "API No Match Prefix",
			host:      "api.example.com",
			path:      "/v2/users",
			wantURL:   "",
			wantFound: false,
		},
		{
			name:      "Frontend Root Match",
			host:      "app.example.com",
			path:      "/dashboard",
			wantURL:   "http://frontend.default.svc.cluster.local:80",
			wantFound: true,
		},
		{
			name:      "Unknown Host",
			host:      "unknown.com",
			path:      "/",
			wantURL:   "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := ctrl.Resolve(context.Background(), tt.host, tt.path)
			require.NoError(t, err)

			if tt.wantFound {
				require.NotNil(t, u)
				require.Equal(t, tt.wantURL, u.String())
			} else {
				require.Nil(t, u)
			}
		})
	}
}

func TestController_ResolverImplementation(t *testing.T) {
	// Verify that Controller implements the BackendResolver interface
	var _ h8sreverse.BackendResolver = &Controller{}
}
