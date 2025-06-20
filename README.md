# h8s - HTTPToNATS - Event-Gapped Ingress

This is a infrastructure solution you may use to get http and websocket traffic into NATS.

It's designed to work with a wasmCloud h8s-provider that integrates with NATS
and implements a incoming-handler towards components. It also works for workloads that connect
directly to NATS and act as responders on "by-convention" subjects.

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
