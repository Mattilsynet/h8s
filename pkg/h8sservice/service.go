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

// Service is a NATS-based client for h8s proxy communication
type Service struct {
	ctx                    context.Context
	wg                     sync.WaitGroup
	natsConn               *nats.Conn
	subjectPrefix          string
	InterestPublishSubject string
	requestServices        map[string]micro.Config
	websocketServices      map[string]string
	Services               []micro.Service
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

// NewClient creates a new h8s server
func NewService(nc *nats.Conn, opts ...Option) *Service {
	client := &Service{
		ctx:               context.Background(),
		natsConn:          nc,
		subjectPrefix:     subjectmapper.SubjectPrefix,
		requestServices:   make(map[string]micro.Config),
		websocketServices: make(map[string]string),
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

func (c *Service) Run() {
	c.wg.Add(2)

	for _, config := range c.requestServices {
		service, err := micro.AddService(c.natsConn, config)
		if err != nil {
			slog.Error("unable to add request service", "error", err, "name", config.Name, "subject", config.Endpoint.Subject)
			c.wg.Done()
		}
		c.Services = append(c.Services, service)
		if err != nil {
			c.wg.Done()
		}
		slog.Info("added request service", "name", config.Name, "subject", config.Endpoint.Subject)
	}

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
			time.Sleep(5 * time.Second) // Publish interest every 30 seconds
		}
	}()

	go func() {
		<-c.ctx.Done()
		for _, service := range c.Services {
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
		Name:     "test",
		Metadata: metadata,
		Version:  "0.0.1",
		Endpoint: &micro.EndpointConfig{
			Subject:    subjectmapper.NewSubjectMapFromParts(host, path, method).PublishSubject(),
			Handler:    micro.HandlerFunc(svc.Handle),
			QueueGroup: fmt.Sprintf("h8ss-%v-%v-%v", method, host, path),
		},
	}
}

func (c *Service) Shutdown() {
	slog.Info("shutting down service")
	c.ctx.Done()
	c.wg.Wait()
	slog.Info("service shutdown complete")
}
