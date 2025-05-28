# h8s - HTTPToNATS - Event-Gapped Ingress

This is a infrastructure solution you may use to get http and websocket traffic into NATS.

It's designed to work with a wasmCloud h8s-provider that integrates with NATS
and implements a incoming-handler towards components. It also works for workloads that connect
directly to NATS and act as responders on "by-convention" subjects.

The motivation for creating this is to never directly expose our wasmcloud environments or other workloads
directly. They should not have any incoming infrastructure requirements.

> *Just egress!*

<!-- end_slide -->

## Diagram 1 - wasmCloud

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

    subgraph wasmCloud
        h8s-provider
        component
    end

    client --> |http| h8s
    h8s --> |pub| subject 
    h8s --> |sub| inbox
    h8s-provider --> |subscribe| subject
    h8s-provider --> |publish| inbox
    h8s-provider --> |link: source_config| component 
    h8s-provider <---> |interface: request-reply| component
```

<!-- end_slide -->
## Diagram 2 - Workloads connected to NATS

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

## Subject Mapping of REST and Websockets

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
            control.ws.conn.established
            control.ws.conn.closed
        end
    end

    subgraph wasmCloud
        h8s-provider
        component
        sender-component
    end

    client --> |ws| h8s
    h8s --> |pub| subject 
    h8s --> |sub| inbox
    h8s --> |pub| control.ws.conn.established
    h8s --> |pub| control.ws.conn.closed
    h8s-provider --> |sub| control.ws.conn.established
    h8s-provider --> |sub| control.ws.conn.closed
    h8s-provider --> |sub| subject
    h8s-provider --> |pub| inbox
    h8s-provider --> |link: source_config| component 
    h8s-provider --> |interface: receiver| component
    sender-component --> |interface: sender| h8s-provider 
```

> The sender WIT interface provides a couple of functions: `'get-connections()' and 'get-connections-by-subject(subject)'.`
