# h8s - HTTPToNATS

This is a infrastructure solution you may use to get http traffic into NATS.
It's designed to work with a wasmCloud h8s-provider that integrates with NATS
and implements the incoming-handler towards components.

The motivation for creating this is to never directly expose our wasmcloud environments
directly. They should not have any incoming requirements.

> *Just egress!*

<!-- end_slide -->

## Diagram

```mermaid +render
graph TB
    client

    subgraph wasmCloud
        h8s-provider
        component
    end

    subgraph edge-infrastructure 
        h8sd
    end

    subgraph NATS-Infrastructure
        subgraph Account
            subject
            inbox
        end
    end

    client --> |http,ws| h8sd
    h8sd --> |pub| subject 
    h8sd --> |sub| inbox
    h8s-provider --> |subscribe| subject
    h8s-provider --> |publish| inbox
    component --> |target_config| h8s-provider
    h8s-provider --> |incoming-handler| component
    component --> |outgoing-handler | h8s-provider 
```

<!-- end_slide -->

## Subject Mapping of REST and Websockets

..wip..
