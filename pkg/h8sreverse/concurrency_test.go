package h8sreverse

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestHTTPRequestsRunConcurrentlyWithBoundedAdmission(t *testing.T) {
	for _, mode := range []string{"host", "all", "host-queue", "all-queue"} {
		t.Run(mode, func(t *testing.T) {
			ns := startEmbeddedNATS(t)
			nc, err := nats.Connect(ns.ClientURL())
			require.NoError(t, err)
			t.Cleanup(func() { _ = nc.Drain() })
			entered := make(chan string, 8)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				entered <- req.URL.Path
				<-release
				w.Header().Set("X-Request-Path", req.URL.Path)
				w.Header().Set("X-Request-Range", req.Header.Get("Range"))
				fmt.Fprint(w, req.URL.Path+":first")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, ":second")
			}))
			t.Cleanup(backend.Close)
			t.Cleanup(unblock)
			proxy := NewReverseProxy(nc)
			require.Equal(t, defaultMaxConcurrentHTTPRequests, cap(proxy.httpRequests))
			proxy.httpRequests = make(chan struct{}, 2)
			backendURL, err := url.Parse(backend.URL)
			require.NoError(t, err)
			proxy.Resolver = &StaticResolver{BackendURL: backendURL}
			if strings.Contains(mode, "queue") {
				proxy.QueueGroup = "test-proxy"
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if strings.HasPrefix(mode, "host") {
				require.NoError(t, proxy.SubscribeForHost(ctx, "video.example.com"))
			} else {
				require.NoError(t, proxy.SubscribeAll(ctx))
			}
			require.NoError(t, nc.FlushTimeout(time.Second))
			first := publishConcurrentRequest(t, nc, "/live/first", false)
			second := publishConcurrentRequest(t, nc, "/live/second", false)
			for range 2 {
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("HTTP request blocked behind another request on the same subscription")
				}
			}
			// The exact-root subscription shares the same limiter as path requests.
			busy := publishConcurrentRequest(t, nc, "/", true)
			reply, err := busy.NextMsg(time.Second)
			require.NoError(t, err)
			require.Equal(t, "503", reply.Header.Get("Status-Code"))
			require.Equal(t, strconv.Itoa(len(reply.Data)), reply.Header.Get("Content-Length"))
			require.Contains(t, string(reply.Data), "concurrency limit")
			_, err = busy.NextMsg(20 * time.Millisecond)
			require.ErrorIs(t, err, nats.ErrTimeout, "finite busy response must not require a separate EOF")
			unblock()
			assertConcurrentResponse(t, first, "/live/first")
			assertConcurrentResponse(t, second, "/live/second")
			require.Eventually(t, func() bool { return len(proxy.httpRequests) == 0 }, time.Second, time.Millisecond)
			recovery := publishConcurrentRequest(t, nc, "/after", false)
			assertConcurrentResponse(t, recovery, "/after")
			require.Eventually(t, func() bool { return len(proxy.httpRequests) == 0 }, time.Second, time.Millisecond)
		})
	}
}

func TestHTTPSubscriptionContextCancelsInFlightRequest(t *testing.T) {
	ns := startEmbeddedNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = nc.Drain() })
	entered, stopped := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-req.Context().Done()
		close(stopped)
	}))
	t.Cleanup(backend.Close)
	proxy := NewReverseProxy(nc)
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	proxy.Resolver = &StaticResolver{BackendURL: backendURL}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, proxy.SubscribeForHost(ctx, "video.example.com"))
	require.NoError(t, nc.FlushTimeout(time.Second))
	replies := publishConcurrentRequest(t, nc, "/live/cancel", false)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("backend not reached")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not inherit subscription context")
	}
	reply, err := replies.NextMsg(time.Second)
	require.NoError(t, err)
	require.Equal(t, "502", reply.Header.Get("Status-Code"))
	require.Equal(t, strconv.Itoa(len(reply.Data)), reply.Header.Get("Content-Length"))
	require.Eventually(t, func() bool { return len(proxy.httpRequests) == 0 }, time.Second, time.Millisecond)
	shutdown := publishConcurrentRequest(t, nc, "/after-cancel", false)
	reply, err = shutdown.NextMsg(time.Second)
	require.NoError(t, err)
	require.Equal(t, "503", reply.Header.Get("Status-Code"))
	require.Equal(t, strconv.Itoa(len(reply.Data)), reply.Header.Get("Content-Length"))
}

func publishConcurrentRequest(t *testing.T, nc *nats.Conn, path string, root bool) *nats.Subscription {
	t.Helper()
	reply := nats.NewInbox()
	sub, err := nc.SubscribeSync(reply)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	subject := "h8s.http.GET.com.example.video"
	if !root {
		subject += "." + strings.TrimPrefix(strings.ReplaceAll(path, "/", "."), ".")
	}
	require.NoError(t, nc.PublishMsg(&nats.Msg{Subject: subject, Reply: reply, Header: nats.Header{
		"X-H8s-Original-Host": {"video.example.com"}, "X-H8s-Original-Path": {path}, "Range": {"bytes=0-31"},
	}}))
	require.NoError(t, nc.FlushTimeout(time.Second))
	return sub
}

func assertConcurrentResponse(t *testing.T, sub *nats.Subscription, path string) {
	t.Helper()
	var body strings.Builder
	first := true
	for {
		message, err := sub.NextMsg(2 * time.Second)
		require.NoError(t, err)
		if first {
			require.Equal(t, "200", message.Header.Get("Status-Code"))
			require.Equal(t, path, message.Header.Get("X-Request-Path"))
			require.Equal(t, "bytes=0-31", message.Header.Get("X-Request-Range"))
			require.Empty(t, message.Header.Get("Content-Length"))
			first = false
		} else {
			require.Empty(t, message.Header, "headers must only appear on the first reply")
		}
		if len(message.Data) == 0 {
			break
		}
		body.Write(message.Data)
	}
	require.Equal(t, path+":first:second", body.String())
}
