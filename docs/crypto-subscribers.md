# Using h8s message encryption in custom NATS subscribers

`github.com/Mattilsynet/h8s/pkg/msgcrypto` is a small standalone wrapper around
NATS `nkeys` curve keys. It does not import NATS or HTTP packages, so it can be
used by any service, not just `h8sservice` or `h8sreverse`.

The encryption contract is deliberately simple:

- A responder advertises an `X...` curve public key as `pub` in its interest.
- `h8sd` seals the HTTP request body to that key and sets `X-H8s-From` to its
  own `X...` public key.
- The responder opens the body with its private key and the sender key from
  `X-H8s-From`.
- The responder seals the response body to the same `X-H8s-From` key.
- If the request has no `X-H8s-From`, the request is plaintext. The response
  must then be plaintext too.

The sealed body is the standard `nkeys` curve format:

```text
"xkv1" || 24-byte random nonce || NaCl box ciphertext
```

There is no separate algorithm, salt, nonce, or key-ID field to manage.

Only `pkg/msgcrypto` is required. The interest examples below use
`pkg/interest` and `pkg/tracker` as convenience helpers; the same JSON can be
published directly if you do not want those dependencies.

## Imports used by the examples

The snippets below assume these imports:

```go
import (
	"context"
	"log/slog"
	"strconv"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/interest"
	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)
```

If you only use the encryption primitive, keep `pkg/msgcrypto` and `nats.go`
and drop the h8s interest helpers.

> [!IMPORTANT]
> `h8sd` only discovers keys when it is started with `--track-interest`.
> Without that flag, `h8sd` does not run an interest tracker and forwards
> requests without encryption.

## 1. Create or load a key

For a single-instance responder, an ephemeral key is sufficient:

```go
key, err := msgcrypto.Generate()
if err != nil {
    return err
}
defer key.Wipe()
```

For multiple replicas behind a queue group, every replica must use the same
seed. Otherwise one replica may receive a request sealed to another replica's
key.

```go
key, err := msgcrypto.FromSeedFile("/run/secrets/h8s-curve.seed")
if err != nil {
    return err
}
defer key.Wipe()
```

The seed file contains the encoded `SX...` seed and must not be group- or
world-readable:

```bash
chmod 0600 /run/secrets/h8s-curve.seed
```

`FromSeedFile` rejects files with group or world permission bits.

## 2. Advertise the public key in an interest

The interest wire format is JSON:

```json
{
  "host": "api.example.com",
  "path": "/hello",
  "method": "GET",
  "pub": "X...",
  "instance": "unique-per-process"
}
```

- `pub` is optional. Omit it to keep the route plaintext.
- `instance` should be stable for the lifetime of the process and unique among
  replicas. `github.com/nats-io/nuid` is a convenient source.
- Publish to `h8s.control.interest.register.<reversed-host>` every five seconds.
- Publish the same payload to
  `h8s.control.interest.unregister.<reversed-host>` on shutdown.
- `h8s.control.interest` is available as
  `h8sproxy.H8SInterestControlSubject`.

The reusable publisher in `pkg/interest` handles the interval and reversed
hostname for you:

```go
instanceID := nuid.Next()

publisher := &interest.Publisher{
    NATSConn: nc,
    Subject:  h8sproxy.H8SInterestControlSubject,
    Entries: func() []tracker.Interest {
        return []tracker.Interest{{
            Host:       "api.example.com",
            Path:       "/hello",
            Method:     "GET",
            PublicKey:  key.PublicKey(),
            InstanceID: instanceID,
        }}
    },
}

ctx, cancel := context.WithCancel(context.Background())
defer func() {
    cancel()
    publisher.Unregister()
}()
go publisher.Run(ctx)
```

If you do not want to depend on `pkg/interest`, publish the JSON directly using
the same subject and five-second interval. `pkg/subjectmapper.ReverseHostname`
produces the reversed hostname.

Legacy payloads that omit both `pub` and `instance` are accepted as plaintext,
but new subscribers should include both fields.

## 3. Handle an HTTP request

The request subject is:

```text
h8s.<scheme>.<method>.<reversed-host>.<path>
```

For `GET https://api.example.com/hello`, that is:

```text
h8s.http.GET.com.example.api.hello
```

A minimal subscriber:

```go
const h8sFromHeader = "X-H8s-From"

func handleRequest(nc *nats.Conn, key *msgcrypto.Key, msg *nats.Msg) {
    from := msg.Header.Get(h8sFromHeader)
    body := msg.Data

    if from != "" {
        plaintext, err := key.Open(body, from)
        if err != nil {
            respond(nc, key, msg.Reply, from, 400, []byte("invalid encrypted request"))
            return
        }
        body = plaintext
    }

    // body is now plaintext regardless of whether the request was sealed.
    _ = body
    respond(nc, key, msg.Reply, from, 200, []byte("hello"))
}

func respond(
    nc *nats.Conn,
    key *msgcrypto.Key,
    reply string,
    from string,
    status int,
    body []byte,
) {
    wire := body
    if from != "" {
        sealed, err := key.Seal(body, from)
        if err != nil {
            slog.Error("failed to seal response", "error", err)
            return
        }
        wire = sealed
    }

    msg := nats.NewMsg(reply)
    msg.Header.Set("Status", strconv.Itoa(status))
    msg.Header.Set("Status-Code", strconv.Itoa(status))
    msg.Header.Set("Content-Type", "text/plain")
    msg.Header.Set("Content-Length", strconv.Itoa(len(body)))
    msg.Data = wire

    if err := nc.PublishMsg(msg); err != nil {
        slog.Error("failed to publish response", "error", err)
    }
}
```

Important details:

- Read `X-H8s-From` before copying or sanitizing headers.
- `Content-Length` must be the **plaintext** length, not the sealed length.
- `Status` and `Status-Code` must be numeric strings such as `"200"`.
- Seal the error body too when the request was sealed. `h8sd` pinned your
  responder key and will reject a plaintext reply to a sealed request.
- If the request had no `X-H8s-From`, do not seal the response.

## 4. Streaming responses

For a finite response, use one message with `Content-Length` as shown above.

For a streaming response:

1. Send the first message with the response headers and the first sealed chunk.
2. Send later chunks sealed independently to the same `from` key.
3. Omit `Content-Length` for streaming responses.
4. Send an empty plaintext chunk as EOF. If the request was sealed, seal the
   empty plaintext; the wire message is non-empty, but `h8sd` opens it and
   treats the resulting empty body as EOF.

Every chunk is opened independently by `h8sd`, so each chunk must be sealed
with the same sender key.

## 5. WebSocket subscribers

The same key and `X-H8s-From` rules apply, but the `From` value is carried on
the control and frame headers.

- The `h8s.control.ws.conn.established.<reversed-host>` control message carries
  `X-H8s-From` in its headers. Store that sender key for the connection, keyed
  by the control message's reply subject.
- WebSocket frames published by `h8sd` carry `X-H8s-From` in their headers.
  Open `msg.Data` with that sender key before treating it as a client frame.
- When publishing a frame back to `h8sd`, seal the payload to the stored
  `X-H8s-From` key.
- A plaintext WebSocket connection has no `X-H8s-From` and must remain
  plaintext.

Do not use a frame's `X-H8s-From` value to select a different key than the one
stored for the connection. Treat it as a consistency check, not as a routing
hint.

## 6. Multiple replicas and restarts

All responders behind the same queue group must use the same seed/key so any
replica can open a request. `h8sd` currently resolves a route to one advertised
key; distinct keys for the same route are treated as a conflict and fail closed
with `502`.

Use a stable seed when restarts should be transparent:

- Same seed across restarts means the same `pub`.
- A restarted instance can open requests that were sealed to the previous
  instance.
- Old interest entries and new entries with the same `pub` are healthy; the
  tracker evicts stale entries after one minute.

An ephemeral key changes the public key on restart. If the old interest entry
is still present, the route temporarily has multiple distinct keys and fails
closed until the old entry is unregistered or evicted.

## 7. Security notes

- Interest messages are plaintext. A principal that can publish to the
  interest subject can advertise a key. Restrict publish permissions with NATS
  ACLs if that matters to your deployment.
- `X-H8s-From` proves possession of the sender key, not authorization to call
  the service.
- There is no replay protection, subject/request binding, or forward secrecy.
  Metadata such as subjects, hosts, paths, and timings remains visible.
- A responder with a key may still receive plaintext requests. If your service
  requires encryption, reject requests without `X-H8s-From` instead of
  accepting them.
- `Wipe()` on shutdown is best effort; process exit also releases the key
  material.

## 8. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Requests are never sealed | `h8sd` is not running with `--track-interest`, or the interest has no `pub` |
| `h8sd` returns `502` | No proxy key, conflicting responder keys, or the responder key set was not resolved |
| `Open` fails | Wrong private key, wrong `From` value, truncated/tampered body, or a different seed than the sender used |
| Response body is empty or truncated | `Content-Length` describes the sealed length instead of the plaintext length |
| Queue-group requests intermittently fail | Replicas are using different seeds/keys |
| Restart causes temporary `502`s | Old interest with a different ephemeral key is still present until unregister/TTL |
