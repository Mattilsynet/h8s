package h8sservice

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// handleRequest decrypts a sealed request and wraps the reply methods so the
// handler only ever sees and produces plaintext. A request without
// X-H8s-From is passed through unchanged: publish-capable NATS principals are
// trusted, and a keyed responder still accepts plaintext.
func (c *Service) handleRequest(request micro.Request, handler RequestServiceHandler) {
	from := request.Headers().Get(h8sproxy.H8SFromHeaderName)
	if from == "" {
		handler.Handle(request)
		return
	}
	if c.CryptoKey == nil {
		slog.Error("received sealed request but service has no crypto key", "subject", request.Subject())
		_ = request.Error("500", "encryption is not configured", nil)
		return
	}

	plaintext, err := c.CryptoKey.Open(request.Data(), from)
	if err != nil {
		slog.Error("failed to open request body", "error", err, "subject", request.Subject())
		// The reply must stay sealed because the proxy pinned this request's
		// responder key and will reject a plaintext response.
		_ = (&cryptoRequest{Request: request, key: c.CryptoKey, recipient: from}).Error(
			"400", "invalid encrypted request", []byte("invalid encrypted request"))
		return
	}

	handler.Handle(&cryptoRequest{
		Request:   request,
		key:       c.CryptoKey,
		recipient: from,
		data:      plaintext,
	})
}

type cryptoRequest struct {
	micro.Request
	key       *msgcrypto.Key
	recipient string
	data      []byte
}

func (r *cryptoRequest) Data() []byte {
	return r.data
}

func (r *cryptoRequest) Respond(data []byte, opts ...micro.RespondOpt) error {
	sealed, err := r.key.Seal(data, r.recipient)
	if err != nil {
		return err
	}
	return r.Request.Respond(sealed, opts...)
}

func (r *cryptoRequest) RespondJSON(value any, opts ...micro.RespondOpt) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.Respond(data, opts...)
}

func (r *cryptoRequest) Error(code, description string, data []byte, opts ...micro.RespondOpt) error {
	sealed, err := r.key.Seal(data, r.recipient)
	if err != nil {
		return err
	}
	return r.Request.Error(code, description, sealed, opts...)
}

func (c *Service) handleWebsocketFrame(handler *WebsocketHandler, msg *nats.Msg) {
	from := msg.Header.Get(h8sproxy.H8SFromHeaderName)
	if from == "" {
		handler.ServiceHandler.Read(msg)
		return
	}
	if c.CryptoKey == nil {
		slog.Error("received sealed WebSocket frame but service has no crypto key", "subject", msg.Subject)
		return
	}

	plaintext, err := c.CryptoKey.Open(msg.Data, from)
	if err != nil {
		slog.Error("failed to open WebSocket frame", "error", err, "subject", msg.Subject)
		return
	}
	copyMsg := *msg
	copyMsg.Data = plaintext
	copyMsg.Header = cloneNATSHeader(msg.Header)
	stripH8SHeaders(copyMsg.Header)
	handler.ServiceHandler.Read(&copyMsg)
}

// Publish sends data to a WebSocket connection. If the connection was
// established with encryption, the frame is sealed to the h8sd key.
func (h *WebsocketHandler) Publish(connSubject string, data []byte) error {
	if h.CryptoKey != nil && h.GetWSConnKey != nil {
		if recipient := h.GetWSConnKey(connSubject); recipient != "" {
			sealed, err := h.CryptoKey.Seal(data, recipient)
			if err != nil {
				return err
			}
			return h.NatsConn.Publish(connSubject, sealed)
		}
	}
	return h.NatsConn.Publish(connSubject, data)
}

// Respond publishes data to the connection that delivered msg.
func (h *WebsocketHandler) Respond(msg *nats.Msg, data []byte) error {
	return h.Publish(msg.Reply, data)
}

func cloneNATSHeader(h nats.Header) nats.Header {
	if h == nil {
		return nil
	}
	cloned := make(nats.Header, len(h))
	for key, values := range h {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func stripH8SHeaders(h nats.Header) {
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "x-h8s-") {
			h.Del(key)
		}
	}
}
