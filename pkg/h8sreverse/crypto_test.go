package h8sreverse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestReverseProxyOpensSealedRequestAndSealsResponse(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	reverseKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer reverseKey.Wipe()
	clientKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer clientKey.Wipe()

	var backendBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		backendBody = body
		_, _ = w.Write([]byte("reverse response"))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	rp := NewReverseProxy(nc, WithCryptoKey(reverseKey))
	rp.FilterHost = "test.local"
	rp.Resolver = &StaticResolver{BackendURL: backendURL}

	ctx := context.Background()
	require.NoError(t, rp.SubscribeForHost(ctx, "test.local"))
	defer rp.UnsubscribeForHost("test.local")

	replySub, err := nc.SubscribeSync("_INBOX.reverse.crypto")
	require.NoError(t, err)

	sealed, err := clientKey.Seal([]byte("sealed request body"), reverseKey.PublicKey())
	require.NoError(t, err)
	msg := &nats.Msg{
		Subject: "h8s.http.POST.local.test.echo",
		Reply:   "_INBOX.reverse.crypto",
		Data:    sealed,
		Header:  nats.Header{},
	}
	msg.Header.Set(h8sproxy.H8SFromHeaderName, clientKey.PublicKey())
	msg.Header.Set("X-H8s-Original-Host", "test.local")
	msg.Header.Set("X-H8s-Original-Path", "/echo")
	require.NoError(t, nc.PublishMsg(msg))
	require.NoError(t, nc.FlushTimeout(time.Second))

	first, err := replySub.NextMsg(3 * time.Second)
	require.NoError(t, err)
	plaintext, err := clientKey.Open(first.Data, reverseKey.PublicKey())
	require.NoError(t, err)
	require.Equal(t, "reverse response", string(plaintext))
	require.Equal(t, "200", first.Header.Get("Status-Code"))
	require.Equal(t, []byte("sealed request body"), backendBody)

	// The stream closes with a sealed empty plaintext message.
	eof, err := replySub.NextMsg(3 * time.Second)
	require.NoError(t, err)
	empty, err := clientKey.Open(eof.Data, reverseKey.PublicKey())
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestReverseProxyPublishesWildcardInterest(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	reverseKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer reverseKey.Wipe()

	rp := NewReverseProxy(nc, WithCryptoKey(reverseKey))

	registerSub, err := nc.SubscribeSync(h8sproxy.H8SInterestControlSubject + ".register.local.test")
	require.NoError(t, err)
	unregisterSub, err := nc.SubscribeSync(h8sproxy.H8SInterestControlSubject + ".unregister.local.test")
	require.NoError(t, err)

	require.NoError(t, rp.SubscribeForHost(context.Background(), "test.local"))

	msg, err := registerSub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var advertised tracker.Interest
	require.NoError(t, json.Unmarshal(msg.Data, &advertised))
	require.Equal(t, "test.local", advertised.Host)
	require.Equal(t, "*", advertised.Path)
	require.Equal(t, "*", advertised.Method)
	require.Equal(t, reverseKey.PublicKey(), advertised.PublicKey)
	require.NotEmpty(t, advertised.InstanceID)

	require.NoError(t, rp.UnsubscribeForHost("test.local"))
	msg, err = unregisterSub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var withdrawn tracker.Interest
	require.NoError(t, json.Unmarshal(msg.Data, &withdrawn))
	require.Equal(t, advertised.InstanceID, withdrawn.InstanceID)
	require.Equal(t, reverseKey.PublicKey(), withdrawn.PublicKey)
}
