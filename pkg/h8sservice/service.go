// Package h8sservice package provides a NATS-based client for handling h8s proxy communication
package h8sservice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Mattilsynet/h8s/pkg/subjectmapper"
	"github.com/Mattilsynet/h8s/pkg/tracker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

type ServiceRequestHandler interface {
	Handle(micro.Request)
}

type ServiceWebsocketHandler interface {
	Read(msg *nats.Msg)
	Write(msg *nats.Msg)
}

type WebsocketHandlerConfig struct {
	Name           string
	Description    string
	Host           string
	Path           string
	ReceiveSubject string
	QueueGroup     string
	Handler        ServiceWebsocketHandler
}

func NewWebsocketHandlerConfig(host string, path string) *WebsocketHandlerConfig {
	sm := subjectmapper.NewWebSocketMap(
		fmt.Sprintf("ws://%v%v", host, path))

	config := &WebsocketHandlerConfig{
		Name:           fmt.Sprintf("ws %v%v", host, path),
		ReceiveSubject: sm.PublishSubject(),
		QueueGroup:     fmt.Sprintf("h8ss-ws-%v%v", host, path),
	}
	return config
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
	websocketServices      map[string]*WebsocketHandlerConfig
	websocketConnections   map[string]string
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
		ctx:               context.Background(),
		natsConn:          nc,
		subjectPrefix:     subjectmapper.SubjectPrefix,
		requestServices:   make(map[string]micro.Config),
		websocketServices: make(map[string]*WebsocketHandlerConfig),
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

func (c *Service) Run() {
	c.wg.Add(3)

	for _, config := range c.requestServices {
		service, err := micro.AddService(c.natsConn, config)
		if err != nil {
			slog.Error("unable to add request service", "error", err, "name", config.Name, "subject", config.Endpoint.Subject)
			c.wg.Done()
		}
		c.requestReplyServices = append(c.requestReplyServices, service)
		if err != nil {
			c.wg.Done()
		}
		slog.Info("added request service", "name", config.Name, "subject", config.Endpoint.Subject)
	}

	// Set up subscribers for incoming data over websockets
	for _, config := range c.websocketServices {
		_, err := c.natsConn.QueueSubscribe(config.ReceiveSubject, config.QueueGroup, config.Handler.Read)
		if err != nil {
			slog.Error("failed to subscribe to websocket subject", "error", err, "subject", config.ReceiveSubject)
		}
	}

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

func (c *Service) AddRequestHandler(host string, path string, method string, svc ServiceRequestHandler) {
	metadata := make(map[string]string)
	metadata["host"] = host
	metadata["path"] = path
	metadata["method"] = method

	c.requestServices[host+path] = micro.Config{
		Name:        fmt.Sprintf("%v %v%v", method, host, path),
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

func (c *Service) AddWebsocketHandler(wssvc *WebsocketHandlerConfig, swh ServiceWebsocketHandler) {
	wssvc.Handler = swh
	c.websocketServices[wssvc.Name] = wssvc
}

func (c *Service) Shutdown() {
	slog.Info("shutting down service")
	c.ctx.Done()
	c.wg.Wait()
	slog.Info("service shutdown complete")
}
