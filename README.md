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
        h8sd
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

    client --> |http,ws| h8sd
    h8sd --> |pub| subject 
    h8sd --> |sub| inbox
    h8s-provider --> |subscribe| subject
    h8s-provider --> |publish| inbox
    h8s-provider --> |source_config| component 
    h8s-provider <---> |incoming-handler| component
    component --> |outgoing-handler | h8s-provider 
```

<!-- end_slide -->
## Diagram 2 - Workloads connected to NATS

```mermaid +render
graph TB 
    client

    subgraph edge-infrastructure 
        h8sd
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

    client --> |http,ws| h8sd
    h8sd --> |pub| subject 
    h8sd --> |sub| inbox
    workload --> |subscribe| subject
    workload --> |publish| inbox
```
<!-- end_slide -->

## Subject Mapping of REST and Websockets

..wip..
