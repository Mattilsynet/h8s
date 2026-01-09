package h8sproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mattilsynet/h8s/internal/natstest"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func BenchmarkProxyHandler_Sequential(b *testing.B) {
	ns := natstest.StartEmbeddedNATSServer(b)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(b, err)
	defer nc.Close()

	proxy := NewH8Sproxy(nc)

	// Subscribe to backend subject
	// Request: GET http://localhost/bench
	// Subject: h8s.http.GET.localhost.bench
	_, err = nc.Subscribe("h8s.http.>", func(msg *nats.Msg) {
		nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    []byte("OK"),
			Header:  nats.Header{"Status": []string{"200 OK"}, "Status-Code": []string{"200"}},
		})
		nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    []byte{},
		})
	})
	require.NoError(b, err)
	nc.Flush()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("GET", "http://localhost/bench", http.NoBody)
		w := httptest.NewRecorder()
		proxy.Handler(w, req)
	}
}

func BenchmarkProxyHandler_Parallel(b *testing.B) {
	ns := natstest.StartEmbeddedNATSServer(b)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(b, err)
	defer nc.Close()

	proxy := NewH8Sproxy(nc)

	_, err = nc.Subscribe("h8s.http.>", func(msg *nats.Msg) {
		nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    []byte("OK"),
			Header:  nats.Header{"Status": []string{"200 OK"}, "Status-Code": []string{"200"}},
		})
		nc.PublishMsg(&nats.Msg{
			Subject: msg.Reply,
			Data:    []byte{},
		})
	})

	require.NoError(b, err)
	nc.Flush()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("GET", "http://localhost/bench", http.NoBody)
			w := httptest.NewRecorder()
			proxy.Handler(w, req)
		}
	})
}
