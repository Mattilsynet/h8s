// Package h8sservice package provides a NATS-based client for handling h8s proxy communication
package h8sservice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/h8sproxy"
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
	ctx                    context.Context
	wg                     sync.WaitGroup
	natsConn               *nats.Conn
	subjectPrefix          string
	InterestPublishSubject string
	ControlMessageSubject  string
	requestServices        map[string]micro.Config
	websocketServices      map[string]*WebsocketHandler
	websocketConnections   *ServiceWebsocketConnections
	requestReplyServices   []micro.Service
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

// NewService creates a new h8s server
func NewService(nc *nats.Conn, opts ...Option) *Service {
	client := &Service{
		ctx:                  context.Background(),
		natsConn:             nc,
		subjectPrefix:        subjectmapper.SubjectPrefix,
		requestServices:      make(map[string]micro.Config),
		websocketServices:    make(map[string]*WebsocketHandler),
		websocketConnections: NewWebSocketConnections(),
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
			c.wg.Done()
		}
		c.requestReplyServices = append(c.requestReplyServices, service)
		slog.Info("added request service", "name", config.Name, "subject", config.Endpoint.Subject)
	}

	// Set up subscribers for incoming data over websockets
	for _, config := range c.websocketServices {
		_, err := c.natsConn.QueueSubscribe(config.ReceiveSubject, config.QueueGroup, config.ServiceHandler.Read)
		if err != nil {
			slog.Error("failed to subscribe to websocket subject", "error", err, "subject", config.ReceiveSubject)
		}
	}

	c.wg.Add(2)
	// Listen for websocket connection events
	go func() {
		if _, err := c.natsConn.Subscribe(
			h8sproxy.H8sControlWebsocketAll,
			func(msg *nats.Msg) {
				switch msg.Subject {
				case h8sproxy.H8SControlWebsocketClosed:
					c.websocketConnections.Delete(msg.Reply)
				case h8sproxy.H8SControlWebsocketEstablished:
					c.websocketConnections.Add(
						msg.Reply,
						msg)
				}
			}); err != nil {
			slog.Error(
				"failed to subscribe to control subject",
				"error", err,
				"subject", h8sproxy.H8SControlSubjectPrefix)
		}
	}()

	// Publish interests periodically
	go func() {
		for {
			for _, config := range c.requestServices {
				interest := tracker.Interest{
					Host:   config.Metadata["host"],
					Path:   config.Metadata["path"],
					Method: config.Metadata["method"],
				}
				bytes, err := json.Marshal(interest)
				if err != nil {
					slog.Error("unable to marshal Interest", "error", err)
					continue
				}
				if err := c.natsConn.Publish(c.InterestPublishSubject, bytes); err != nil {
					slog.Error("unable to publish Interest", "error", err, "subject", config.Endpoint.Subject)
				}
			}

			for _, config := range c.websocketServices {
				interest := tracker.Interest{
					Host:   config.Host,
					Path:   config.Path,
					Method: "ws",
				}
				bytes, err := json.Marshal(interest)
				if err != nil {
					slog.Error("unable to marshal Interest", "error", err)
					continue
				}
				if err := c.natsConn.Publish(c.InterestPublishSubject, bytes); err != nil {
					slog.Error("unable to publish Interest", "error", err, "subject", config.ReceiveSubject)
				}

			}

			time.Sleep(5 * time.Second) // Publish interest every 30 seconds
		}
	}()

	go func() {
		<-c.ctx.Done()
		for _, service := range c.requestReplyServices {
			service.Stop()
		}
		c.wg.Done()
	}()
}

func (c *Service) AddRequestHandler(host string, path string, method string, svc RequestServiceHandler) {
	metadata := make(map[string]string)
	metadata["host"] = host
	metadata["path"] = path
	metadata["method"] = method

	c.requestServices[host+path] = micro.Config{
		Name:        nuid.New().Next(),
		Metadata:    metadata,
		Version:     "0.0.1",
		Description: fmt.Sprintf("%s https://%s%s", method, host, path),
		Endpoint: &micro.EndpointConfig{
			Subject:    subjectmapper.NewSubjectMapFromParts(host, path, method).PublishSubject(),
			Handler:    micro.HandlerFunc(svc.Handle),
			QueueGroup: fmt.Sprintf("h8ss-%v-%v-%v", method, host, path),
		},
	}
}

func (c *Service) RegisterWebsocketHandler(handler *WebsocketHandler) {
	handler.Name = nuid.New().Next()
	handler.NatsConn = c.natsConn
	handler.GetWSConns = c.websocketConnections.GetConnections
	handler.GetWSConnsBySubject = c.websocketConnections.GetConnectionsBySubject
	c.websocketServices[handler.ReceiveSubject] = handler
}

func (c *Service) Shutdown() {
	slog.Info("shutting down service")
	c.ctx.Done()
	c.wg.Wait()
	slog.Info("service shutdown complete")
}
