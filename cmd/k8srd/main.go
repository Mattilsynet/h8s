package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Mattilsynet/h8s/pkg/h8sreverse"
	"github.com/Mattilsynet/h8s/pkg/ingress"
	"github.com/nats-io/nats.go"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	natsURLFlag    = flag.String("nats-url", os.Getenv("NATS_URL"), "NATS server URL")
	kubeconfigFlag = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
)

func main() {
	handler := slog.NewJSONHandler(os.Stdout, nil)
	slog.SetDefault(slog.New(handler))
	flag.Parse()

	// 1. Connect to NATS
	natsURL := *natsURLFlag
	if natsURL == "" {
		natsURL = "nats://0.0.0.0:4222"
	}
	nc, err := nats.Connect(natsURL)
	if err != nil {
		slog.Error("Failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer nc.Close()
	slog.Info("Connected to NATS", "url", natsURL)

	// 2. Connect to Kubernetes
	var config *rest.Config
	if *kubeconfigFlag != "" {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfigFlag)
	} else {
		// Try In-Cluster config first
		config, err = rest.InClusterConfig()
		if err != nil {
			// Fallback to default ~/.kube/config if local
			if home := homedir.HomeDir(); home != "" {
				kubeconfig := filepath.Join(home, ".kube", "config")
				config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
			}
		}
	}
	if err != nil {
		slog.Error("Failed to build K8s config", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		slog.Error("Failed to create K8s client", "error", err)
		os.Exit(1)
	}

	// 3. Initialize Proxy with Dynamic Logic
	proxy := h8sreverse.NewReverseProxy(nc)

	// 4. Initialize Ingress Controller (This links K8s -> Proxy)
	ctrl := ingress.NewController(clientset, proxy)

	// Inject the controller as the Resolver for the proxy
	proxy.Resolver = ctrl

	// 5. Start the Controller (Starts watching K8s)
	ctx := context.Background()
	go func() {
		if err := ctrl.Run(ctx); err != nil {
			slog.Error("Ingress controller died", "error", err)
			os.Exit(1)
		}
	}()

	slog.Info("k8srd (Kubernetes Reverse Daemon) started")

	// 6. Block forever
	select {}
}
