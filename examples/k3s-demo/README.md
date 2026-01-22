# h8s Ingress Demo

This demo simulates a full end-to-end "Reverse Tunnel" Ingress architecture.

## Architecture

1.  **Public Internet (`h8sd`)**: Listens on `localhost:8080`.
2.  **Tunnel (`NATS`)**: Transports traffic securely.
3.  **Cluster (`k3s`)**: A private Kubernetes cluster running in Docker.
4.  **Ingress Controller (`k8srd`)**: Runs inside K3s, connects outbound to NATS, and routes traffic to pods.

## Prerequisites

*   Docker & Docker Compose
*   Go (to build the image locally)

## Running the Demo

### 1. Start the Environment
```bash
docker-compose up -d
```

### 2. Build and Import the Controller Image
Since K3s is running in Docker, it can't see your local Docker images. We must import it.

```bash
# Build the image
docker build -t h8s/k8srd:latest -f ../../Dockerfile.k8srd ../..

# Save it to a tarball
docker save h8s/k8srd:latest -o k8srd.tar

# Import into the K3s container
docker cp k8srd.tar k3s-demo-k3s-1:/k8srd.tar
docker exec k3s-demo-k3s-1 ctr images import /k8srd.tar
```

### 3. Deploy the Ingress Controller
We will apply the manifests mounted in `/manifests` inside the K3s container.

```bash
docker exec k3s-demo-k3s-1 kubectl apply -f /manifests/
```

### 4. Deploy a Sample App (Whoami)

```bash
docker exec k3s-demo-k3s-1 kubectl create deployment whoami --image=containous/whoami
docker exec k3s-demo-k3s-1 kubectl expose deployment whoami --port=80
```

### 5. Create an Ingress Rule

```bash
docker exec k3s-demo-k3s-1 kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: whoami-ingress
spec:
  rules:
  - host: whoami.localhost
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: whoami
            port:
              number: 80
EOF
```

### 6. Test It!
Send a request to the **Edge Gateway** (`localhost:8080`) with the correct Host header.

```bash
curl -v -H "Host: whoami.localhost" http://localhost:8080
```

You should see the output from the `whoami` pod running inside the isolated K3s cluster!
