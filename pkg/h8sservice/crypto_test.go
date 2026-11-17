package h8sservice

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Mattilsynet/h8s/internal/natstest"
	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"github.com/stretchr/testify/require"
)

type echoHandler struct{}

func (echoHandler) Handle(r micro.Request) {
	_ = r.Respond(r.Data())
}

func TestServiceOpensSealedRequestAndSealsResponse(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	serviceKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer serviceKey.Wipe()
	clientKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer clientKey.Wipe()

	client := NewService(nc, WithCryptoKey(serviceKey))
	client.AddRequestHandler("crypto.local", "/echo", "POST", "http", echoHandler{})

	interestSub, err := nc.SubscribeSync(h8sproxy.H8SInterestControlSubject + ".register.local.crypto")
	require.NoError(t, err)

	go client.Run()
	defer client.Shutdown()

	interestMsg, err := interestSub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var interest tracker.Interest
	require.NoError(t, json.Unmarshal(interestMsg.Data, &interest))
	require.Equal(t, serviceKey.PublicKey(), interest.PublicKey)
	require.NotEmpty(t, interest.InstanceID)

	subject := subjectmapper.NewSubjectMap(httpRequestFor("crypto.local", "/echo", "POST")).PublishSubject()
	sealed, err := clientKey.Seal([]byte("sealed service request"), serviceKey.PublicKey())
	require.NoError(t, err)

	request := &nats.Msg{
		Subject: subject,
		Data:    sealed,
		Header:  nats.Header{},
	}
	request.Header.Set(h8sproxy.H8SFromHeaderName, clientKey.PublicKey())

	response, err := nc.RequestMsg(request, 2*time.Second)
	require.NoError(t, err)
	plaintext, err := clientKey.Open(response.Data, serviceKey.PublicKey())
	require.NoError(t, err)
	require.Equal(t, []byte("sealed service request"), plaintext)
}

func httpRequestFor(host, path, method string) *http.Request {
	u, _ := url.Parse("http://" + host + path)
	return &http.Request{Host: host, Method: method, URL: u}
}

func TestServiceAcceptsPlaintextWhenKeyConfigured(t *testing.T) {
	ns := natstest.StartEmbeddedNATSServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Drain()

	serviceKey, err := msgcrypto.Generate()
	require.NoError(t, err)
	defer serviceKey.Wipe()

	client := NewService(nc, WithCryptoKey(serviceKey))
	client.AddRequestHandler("plain.local", "/echo", "POST", "http", echoHandler{})

	interestSub, err := nc.SubscribeSync(h8sproxy.H8SInterestControlSubject + ".register.local.plain")
	require.NoError(t, err)

	go client.Run()
	defer client.Shutdown()

	_, err = interestSub.NextMsg(2 * time.Second)
	require.NoError(t, err)

	subject := subjectmapper.NewSubjectMap(httpRequestFor("plain.local", "/echo", "POST")).PublishSubject()
	response, err := nc.Request(subject, []byte("plaintext request"), 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, []byte("plaintext request"), response.Data)
}
