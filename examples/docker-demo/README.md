# H8S Docker Compose Demo

This demo runs the full H8S stack using Docker Compose, pulling `h8sd` and `h8srd` images from GitHub Container Registry (GHCR).
It uses `mccutchen/go-httpbin` as the backend to demonstrate HTTP, WebSocket, and SSE capabilities.

## Components

- **nats**: The messaging backbone.
- **h8sd**: The ingress daemon, exposing port 80.
- **backend**: `kennethreitz/httpbin` (standard httpbin service).
- **h8srd-backend**: The reverse proxy sidecar that tunnels traffic from NATS to the backend.

## Prerequisites

- Docker and Docker Compose installed.
- **Note**: If the GHCR images are private, you must be logged in to Docker with a PAT (Personal Access Token) that has `read:packages` scope.

## Running the Demo

1. Start the stack:
   ```bash
   docker compose up
   ```

2. **Test Scenarios**:

   **Basic GET**:
   ```bash
   curl -v http://localhost:8080/get
   ```

   **POST Request** (echoes body):
   ```bash
   curl -v -X POST -H "Content-Type: application/json" -d '{"hello": "world"}' http://localhost:8080/post
   ```

   **Status Codes**:
   ```bash
   # Test 418 I'm a teapot
   curl -v http://localhost:8080/status/418

   # Test 500 Internal Server Error
   curl -v http://localhost:8080/status/500
   ```

   **Header Inspection**:
   ```bash
   # See what headers h8s/h8srd forwards
   curl -v -H "X-Custom-Header: test" http://localhost:8080/headers
   ```

   **Response Formats**:
   ```bash
   # JSON response
   curl -v http://localhost:8080/json

   # HTML response
   curl -v http://localhost:8080/html

   # XML response
   curl -v http://localhost:8080/xml
   ```

   **Redirection**:
   ```bash
   # Should redirect to /get
   curl -v -L http://localhost:8080/redirect-to?url=/get
   ```

   **Image**:
   ```bash
   # Fetch a PNG image (binary data)
   curl -v http://localhost:8080/image/png --output test.png
   ```

3. Stop the stack:
   ```bash
   docker compose down
   ```
