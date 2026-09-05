package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestServeWaitsForActiveRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "completed")
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, listener, handler, testLogger(), time.Second) }()
	result := make(chan string, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			result <- err.Error()
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		result <- string(body)
	}()
	<-started
	cancel()
	select {
	case err := <-stopped:
		close(release)
		t.Fatalf("server exited before request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if body := <-result; body != "completed" {
		t.Fatalf("body=%q", body)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestServeClosesRequestAfterGracePeriod(t *testing.T) {
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, listener, handler, testLogger(), 20*time.Millisecond) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server exceeded shutdown grace period")
	}
	<-requestDone
}

func TestProxyWebSocketTunnelAndShutdown(t *testing.T) {
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("incorrect upgrade authentication")
		}
		connection, buffer, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		_ = buffer.Flush()
		frame := make([]byte, 8)
		if _, err := io.ReadFull(buffer, frame); err != nil {
			t.Error(err)
			return
		}
		// A masked client text frame containing "hi" uses a zero mask in this fixture.
		if string(frame) != string([]byte{0x81, 0x82, 0, 0, 0, 0, 'h', 'i'}) {
			t.Errorf("unexpected frame: %x", frame)
		}
		_, _ = connection.Write([]byte{0x81, 2, 'h', 'i'})
		_, _ = io.Copy(io.Discard, connection)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	oauth := &oauthManager{credential: oauthCredential{Access: "access", Refresh: "refresh", AccountID: "account", Expires: time.Now().Add(time.Hour).UnixMilli()}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() {
		stopped <- serve(ctx, listener, newHandler(config{upstream: upstreamURL, proxyToken: "secret"}, oauth, testLogger()), testLogger(), time.Second)
	}()
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = io.WriteString(connection, "GET /responses HTTP/1.1\r\nHost: proxy.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nProxy-Authorization: Bearer secret\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 {
		t.Fatalf("upgrade status=%d", response.StatusCode)
	}
	_, _ = connection.Write([]byte{0x81, 0x82, 0, 0, 0, 0, 'h', 'i'})
	frame := make([]byte, 4)
	if _, err := io.ReadFull(reader, frame); err != nil {
		t.Fatal(err)
	}
	if string(frame) != string([]byte{0x81, 2, 'h', 'i'}) {
		t.Fatalf("frame=%x", frame)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("upgrade connection remained open")
	}
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream upgrade remained open")
	}
}

func TestRewriteModelsRejectsOversizedResponse(t *testing.T) {
	response := &http.Response{StatusCode: 200, Request: httptest.NewRequest("GET", "https://upstream.test/models", nil), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxCatalogBytes+1)))}
	if err := rewriteModelsResponse(response); err == nil {
		t.Fatal("accepted oversized catalog")
	}
}
