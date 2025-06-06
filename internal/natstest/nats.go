package natstest

import (
	"testing"
	"time"

	server "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/require"
)

func StartEmbeddedNATSServer(t *testing.T) *server.Server {
	opts := &server.Options{
		Port: 4223,
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err)

	go ns.Start()
	if !ns.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS server not ready in time")
	}

	t.Cleanup(func() {
		ns.Shutdown()
	})

	return ns
}
