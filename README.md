# h8s - HTTPToNATS - Event-Gapped Ingress

This is a infrastructure solution you may use to get http and websocket traffic into NATS.

It is designed for workloads that connect directly to NATS and act as responders on "by-convention" subjects.

The motivation for creating this is to never directly expose our wasmcloud environments or other workloads
directly. They should not have any incoming infrastructure requirements.

> *Just egress!*

<!-- end_slide -->

## Diagram - Vision 

```mermaid +render
graph LR

    client
    geodns
    subgraph IngressA
        h8s-a
    end

    subgraph IngressB
        h8s-b
    end

    subgraph IngressC
        h8s-c
    end

    subgraph NATS
        direction TB
        subgraph NATS-A
            NATS-A-Account[NATSAccount]
        end
        subgraph NATS-B
            NATS-B-Account[NATSAccount]
        end
    end

    subgraph Cloud 
       CloudApp[YourApp<br>/endpoint1]
    end

    subgraph DC
        DCApp[YourApp<br>/endpoint2]
    end

    subgraph wasmCloud 
        subgraph Cloud 
            WCApp1[YourApp<br>/endpoint3]
        end
        subgraph DC 
            WCApp2[YourApp<br>/endpoint4]
        end
    end

    client --> geodns
    client -..->|http,ws| h8s-a
    client -..->|http,ws| h8s-b
    client -..->|http,ws| h8s-c
    
    NATS-A ==> |gateway| NATS-B
    NATS-B ==> |gateway| NATS-A

    h8s-a -...->|pub,sub| NATS
    h8s-b -...->|pub,sub| NATS
    h8s-c -...->|pub,sub| NATS

    CloudApp -.->|pub,sub| NATS
    DCApp -.->|pub,sub| NATS
    WCApp1 -.->|pub,sub| NATS
    WCApp2 -.->|pub,sub| NATS
    
```


## Diagram 1 - Workloads connected to NATS

```mermaid +render
graph TB 
    client

    subgraph edge-infrastructure 
        h8s
    end

    subgraph NATS-Infrastructure
        subgraph Account
            subject
            inbox
        end
    end

    subgraph workloads-with-nats
        workload
    end

    client --> |http,ws| h8s
    h8s --> |pub| subject 
    h8s --> |sub| inbox
    workload --> |subscribe| subject
    workload --> |publish| inbox
```
<!-- end_slide -->

## Diagram 2 - Workloads connected via h8srd (Reverse Daemon)

```mermaid +render
graph TB 
    client[HTTP Client]

    subgraph edge-infrastructure 
        h8sd[h8sd]
    end

    subgraph NATS-Infrastructure
        subgraph Account
            subject
            inbox
            control[h8s.control.ws.conn.*]
        end
    end

    subgraph Internal-Network
        h8srd
        backend-service[Internal Service<br>:80 or :443]
    end

    client --> |http,ws| h8sd
    h8sd --> |pub| subject 
    h8sd --> |sub| inbox

    h8srd --> |sub| subject
    h8srd --> |pub| inbox
    h8srd --> |sub| control

    h8srd --> |http,ws| backend-service

    note[h8srd acts as a reverse proxy,<br>connecting to local HTTP/WS services<br>via NATS messages]
```
<!-- end_slide -->



## Running the Daemons

### h8sd (Ingress Proxy)

`h8sd` is the ingress point that accepts HTTP/WS connections and forwards them to NATS.

```bash
# Basic usage
./h8sd --nats-url="nats://localhost:4222"

# With credentials and OpenTelemetry
./h8sd \
  --nats-url="nats://demo.nats.io:4222" \
  --nats-creds="./user.creds" \
  --otel-enabled=true \
  --otel-endpoint="localhost:4317"

# With security option (Max Body Size, Host Filters, Request Timeout, Allowed Origins)
./h8sd \
  --nats-url="nats://localhost:4222" \
  --max-body-size=10485760 \
  --host-filters="example.com,api.example.com" \
  --allowed-origins="https://app.example.com,https://admin.example.com" \
  --request-timeout=5s
```

`--allowed-origins` is optional; if omitted, WebSocket origins are not restricted.

### h8srd (Reverse Daemon)

`h8srd` runs alongside your backend service, subscribes to NATS subjects, and forwards requests to your local service.

```bash
# Basic usage
./h8srd --nats-url="nats://localhost:4222"

# With credentials
./h8srd \
  --nats-url="nats://demo.nats.io:4222" \
  --nats-creds="./user.creds"
  --nats-creds="./user.creds"
```

## Performance & Benchmarking

h8s includes a built-in benchmark suite to verify throughput and latency characteristics. The benchmarks test the core `h8sproxy` handler and NATS dispatcher loop.

To run the benchmarks:

```bash
go test -bench=. -benchmem -v ./pkg/h8sproxy
```

### Recent Results (Ryzen 7 7700X)

- **Sequential Request Processing**: ~68 µs/op (~14,000 req/sec per core)
- **Parallel Request Processing**: ~17 µs/op (~58,000 req/sec)

These benchmarks confirm that `h8sd` adds minimal overhead, with the primary bottleneck pushed to the downstream backend service.

