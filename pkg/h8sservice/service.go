// Package h8sservice package provides a NATS-based client for handling h8s proxy communication
package h8sservice

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
	"github.com/Mattilsynet/h8s/pkg/interest"
	"github.com/Mattilsynet/h8s/pkg/msgcrypto"
	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"github.com/nats-io/nuid"
)

type RequestServiceHandler interface {
	Handle(micro.Request)
}
type WebsocketServiceHandler interface {
	Read(msg *nats.Msg)
	Writer()
}

type (
	ServiceWebsocketGetConnectionsFunc          func() []string
	ServiceWebsocketGetConnectionsBySubjectFunc func(subject string) []string
)

type WebsocketHandler struct {
	NatsConn            *nats.Conn
	Name                string
	Description         string
	Host                string
	Path                string
	ReceiveSubject      string
	QueueGroup          string
	ServiceHandler      WebsocketServiceHandler
	GetWSConns          ServiceWebsocketGetConnectionsFunc
	GetWSConnsBySubject ServiceWebsocketGetConnectionsBySubjectFunc
	GetWSConnKey        func(connSubject string) string
	CryptoKey           *msgcrypto.Key
}

func NewWebsocketHandler(service WebsocketServiceHandler, host string, path string) *WebsocketHandler {
	sm := subjectmapper.NewWebSocketMap(
		fmt.Sprintf("ws://%v%v", host, path))

	config := &WebsocketHandler{
		Name:           nuid.New().Next(),
		Host:           host,
		Path:           path,
		ReceiveSubject: sm.PublishSubject(),
		QueueGroup:     fmt.Sprintf("h8ss-ws-%v%v", host, path),
		ServiceHandler: service,
	}
	return config
}

type ServiceWebsocketConnections struct {
	sync.RWMutex
	conns map[string]*nats.Msg
}

func NewWebSocketConnections() *ServiceWebsocketConnections {
	return &ServiceWebsocketConnections{
		conns: make(map[string]*nats.Msg),
	}
}

func (c *ServiceWebsocketConnections) Add(id string, msg *nats.Msg) {
	c.Lock()
	defer c.Unlock()
	c.conns[id] = msg
}

func (c *ServiceWebsocketConnections) Delete(id string) {
	c.Lock()
	defer c.Unlock()
	delete(c.conns, id)
}

func (c *ServiceWebsocketConnections) GetConnections() []string {
	c.RLock()
	defer c.RUnlock()

	var conns []string
	for k := range c.conns {
		conns = append(conns, k)
	}
	return conns
}

// GetConnectionPublicKey returns the h8sd curve public key advertised on the
// WebSocket control message for connSubject. Empty means the session is
// plaintext.
func (c *ServiceWebsocketConnections) GetConnectionPublicKey(connSubject string) string {
	c.RLock()
	defer c.RUnlock()
	if msg, ok := c.conns[connSubject]; ok {
		return msg.Header.Get(h8sproxy.H8SFromHeaderName)
	}
	return ""
}

func (c *ServiceWebsocketConnections) GetConnectionsBySubject(subject string) []string {
	c.RLock()
	defer c.RUnlock()

	var conns []string
	for k, v := range c.conns {
		if v.Header.Get("X-H8S-PublishSubject") == subject {
			conns = append(conns, k)
		}
	}
	return conns
}

// Service is a server-client for the h8s proxy
type Service struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	natsConn               *nats.Conn
	subjectPrefix          string
	InterestPublishSubject string
	ControlMessageSubject  string
	requestServices        map[string]micro.Config
	websocketServices      map[string]*WebsocketHandler
	websocketConnections   *ServiceWebsocketConnections
	requestReplyServices   []micro.Service
	// CryptoKey is the service-wide curve keypair. All replicas behind a
	// queue group must configure the same seed so any replica can open any
	// request. Nil means the service advertises no key and stays plaintext.
	CryptoKey *msgcrypto.Key
	// InstanceID is random per process and stable for its lifetime. It is
	// required in every interest registration and unregistration.
	InstanceID string
}

// Option is a function that can be used to configure the Client
type Option func(*Service)

// WithSubjectPrefix sets a custom subject prefix for the client
func WithSubjectPrefix(prefix string) Option {
	return func(c *Service) {
		c.subjectPrefix = prefix
	}
}

func WithInterestPublishSubject(subject string) Option {
	return func(c *Service) {
		c.InterestPublishSubject = subject
	}
}

// WithCryptoKey enables encryption for this service. Every replica of the
// same service (same queue group) must use the same seed. A service without a
// key advertises no public key and remains plaintext.
func WithCryptoKey(key *msgcrypto.Key) Option {
	return func(c *Service) {
		c.CryptoKey = key
	}
}

// NewService creates a new h8s server
func NewService(nc *nats.Conn, opts ...Option) *Service {
	ctx, cancel := context.WithCancel(context.Background())

	client := &Service{
		ctx:                    ctx,
		cancel:                 cancel,
		natsConn:               nc,
		subjectPrefix:          subjectmapper.SubjectPrefix,
		InterestPublishSubject: h8sproxy.H8SInterestControlSubject,
		requestServices:        make(map[string]micro.Config),
		websocketServices:      make(map[string]*WebsocketHandler),
		websocketConnections:   NewWebSocketConnections(),
		InstanceID:             nuid.New().Next(),
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

func (c *Service) GetWebsocketConnections() []string {
	return nil
}

func (c *Service) Run() {
	for _, config := range c.requestServices {
		service, err := micro.AddService(c.natsConn, config)
		if err != nil {
			slog.Error("unable to add request service", "error", err, "name", config.Name, "subject", config.Endpoint.Subject)
			continue
		}
		c.requestReplyServices = append(c.requestReplyServices, service)
		slog.Info("added request service", "name", config.Name, "subject", config.Endpoint.Subject)
	}

	// Set up subscribers for incoming data over websockets
	for _, config := range c.websocketServices {
		_, err := c.natsConn.QueueSubscribe(config.ReceiveSubject, config.QueueGroup, func(msg *nats.Msg) {
			c.handleWebsocketFrame(config, msg)
		})
		if err != nil {
			slog.Error("failed to subscribe to websocket subject", "error", err, "subject", config.ReceiveSubject)
		}
	}

	c.wg.Add(2)
	// Listen for websocket connection events, scoped to hosts this service handles.
	go func() {
		defer c.wg.Done()

		// Collect unique hosts from registered handlers
		hostSet := make(map[string]struct{})
		for _, config := range c.websocketServices {
			hostSet[config.Host] = struct{}{}
		}
		for _, config := range c.requestServices {
			if h, ok := config.Metadata["host"]; ok {
				hostSet[h] = struct{}{}
			}
		}

		controlHandler := func(msg *nats.Msg) {
			switch {
			case strings.HasPrefix(msg.Subject, h8sproxy.H8SControlWebsocketClosedPrefix):
				c.websocketConnections.Delete(msg.Reply)
			case strings.HasPrefix(msg.Subject, h8sproxy.H8SControlWebsocketEstablishedPrefix):
				c.websocketConnections.Add(
					msg.Reply,
					msg)
			}
		}

		// Subscribe to host-specific control subjects
		for host := range hostSet {
			reversedHost := subjectmapper.ReverseHostname(host)
			estSubject := h8sproxy.H8SControlWebsocketEstablishedPrefix + "." + reversedHost
			closedSubject := h8sproxy.H8SControlWebsocketClosedPrefix + "." + reversedHost
			if _, err := c.natsConn.Subscribe(estSubject, controlHandler); err != nil {
				slog.Error("failed to subscribe to WS established control subject",
					"error", err, "subject", estSubject)
			}
			if _, err := c.natsConn.Subscribe(closedSubject, controlHandler); err != nil {
				slog.Error("failed to subscribe to WS closed control subject",
					"error", err, "subject", closedSubject)
			}
			slog.Info("subscribed to host-scoped WS control subjects", "host", host)
		}

		<-c.ctx.Done()
	}()

	// Publish interests periodically. The publisher publishes once
	// immediately, then every 5 seconds.
	go func() {
		defer c.wg.Done()
		c.newInterestPublisher().Run(c.ctx)
	}()

	go func() {
		<-c.ctx.Done()
		for _, service := range c.requestReplyServices {
			service.Stop()
		}
	}()
}

// AddRequestHandler registers a handler for the given (host, path, method).
// It MUST be called before Run(); the underlying map is not synchronized,
// and Run() iterates it from background goroutines. Calling AddRequestHandler
// concurrently with Run() is a data race.
func (c *Service) AddRequestHandler(host string, path string, method string, scheme string, svc RequestServiceHandler) {
	metadata := make(map[string]string)
	metadata["host"] = host
	metadata["path"] = path
	metadata["method"] = method
	metadata["scheme"] = scheme

	req := &http.Request{
		Method: method,
		Host:   host,
		URL: &url.URL{
			Scheme: scheme,
			Path:   path,
		},
	}

	handler := micro.HandlerFunc(svc.Handle)
	if c.CryptoKey != nil {
		handler = micro.HandlerFunc(func(request micro.Request) {
			c.handleRequest(request, svc)
		})
	}

	c.requestServices[method+"|"+host+path] = micro.Config{
		Name:        nuid.New().Next(),
		Metadata:    metadata,
		Version:     "0.0.1",
		Description: fmt.Sprintf("%s %s://%s%s", method, scheme, host, path),
		Endpoint: &micro.EndpointConfig{
			Subject:    subjectmapper.NewSubjectMap(req).PublishSubject(),
			Handler:    handler,
			QueueGroup: fmt.Sprintf("h8ss-%v-%v-%v", method, host, path),
		},
	}
}

func (c *Service) RegisterWebsocketHandler(handler *WebsocketHandler) {
	handler.Name = nuid.New().Next()
	handler.NatsConn = c.natsConn
	handler.GetWSConns = c.websocketConnections.GetConnections
	handler.GetWSConnsBySubject = c.websocketConnections.GetConnectionsBySubject
	handler.GetWSConnKey = c.websocketConnections.GetConnectionPublicKey
	handler.CryptoKey = c.CryptoKey
	c.websocketServices[handler.ReceiveSubject] = handler
}

func (c *Service) Shutdown() {
	slog.Info("shutting down service")

	// Stop the periodic publisher before unregistering. Otherwise a tick that
	// races with shutdown can publish a register after the unregister and keep
	// the route alive in the tracker until TTL eviction.
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()

	// Fire-and-forget: publish unregister messages for all interests. This
	// gives the tracker immediate removal instead of waiting for TTL eviction.
	c.newInterestPublisher().Unregister()

	slog.Info("service shutdown complete")
}

func (c *Service) newInterestPublisher() *interest.Publisher {
	return &interest.Publisher{
		NATSConn: c.natsConn,
		Subject:  c.InterestPublishSubject,
		Entries:  c.interests,
	}
}

func (c *Service) interests() []tracker.Interest {
	publicKey := ""
	if c.CryptoKey != nil {
		publicKey = c.CryptoKey.PublicKey()
	}

	interests := make([]tracker.Interest, 0, len(c.requestServices)+len(c.websocketServices))
	for _, config := range c.requestServices {
		interests = append(interests, tracker.Interest{
			Host:       config.Metadata["host"],
			Path:       config.Metadata["path"],
			Method:     config.Metadata["method"],
			PublicKey:  publicKey,
			InstanceID: c.InstanceID,
		})
	}
	for _, config := range c.websocketServices {
		interests = append(interests, tracker.Interest{
			Host:       config.Host,
			Path:       config.Path,
			Method:     "ws",
			PublicKey:  publicKey,
			InstanceID: c.InstanceID,
		})
	}
	return interests
}
