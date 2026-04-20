# H8S Local Docker Compose Demo

This demo runs the full H8S stack using Docker Compose, **building `h8sd` and `h8srd` images from the local source code**.
This allows for rapid testing of local changes without the need to push versioned images to a remote registry.
It uses `kennethreitz/httpbin` as the backend to demonstrate HTTP, WebSocket, and SSE capabilities.

> [!IMPORTANT]
> This demo uses per-hostname `h8srd` routing. `H8SRD_HOSTNAME` is required and is set in `docker-compose.yml`.
> In this demo the value is `localhost:8080`.

## Components

- **nats**: The messaging backbone.
- **h8sd**: The ingress daemon, exposing port 8080. Built from local `../../Dockerfile.h8sd`.
- **backend**: `kennethreitz/httpbin` (standard httpbin service).
- **h8srd-backend**: The reverse proxy sidecar that tunnels traffic from NATS to the backend. Built from local `../../Dockerfile.h8srd`.

## Prerequisites

- Docker and Docker Compose installed.

## Running the Demo

1. Start the stack (this will build the images):
   ```bash
   docker compose up --build
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
