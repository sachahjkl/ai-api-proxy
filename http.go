package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	maxRequestBytes = 32 << 20
	maxCatalogBytes = 16 << 20
)

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("body exceeds size limit")
	}
	return body, nil
}

// Track upgraded connections because http.Server.Shutdown does not close them.
type connectionTracker struct {
	mu          sync.Mutex
	connections map[net.Conn]struct{}
}

type trackedListener struct {
	net.Listener
	tracker *connectionTracker
}

type trackedConnection struct {
	net.Conn
	tracker *connectionTracker
}

func (listener trackedListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConnection{Conn: connection, tracker: listener.tracker}
	listener.tracker.mu.Lock()
	listener.tracker.connections[tracked] = struct{}{}
	listener.tracker.mu.Unlock()
	return tracked, nil
}

func (connection *trackedConnection) Close() error {
	err := connection.Conn.Close()
	connection.tracker.mu.Lock()
	delete(connection.tracker.connections, connection)
	connection.tracker.mu.Unlock()
	return err
}

func (tracker *connectionTracker) close() {
	tracker.mu.Lock()
	connections := make([]net.Conn, 0, len(tracker.connections))
	for connection := range tracker.connections {
		connections = append(connections, connection)
	}
	tracker.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger, grace time.Duration) error {
	tracker := &connectionTracker{connections: make(map[net.Conn]struct{})}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	finished := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				logger.Error("shutdown failed", "error", err)
			}
			tracker.close()
		case <-finished:
		}
	}()
	err := server.Serve(trackedListener{Listener: listener, tracker: tracker})
	close(finished)
	<-stopped
	tracker.close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Unwrap preserves ResponseController support for flushing and connection upgrades.
type responseLog struct {
	http.ResponseWriter
	status int
}

func (response *responseLog) Unwrap() http.ResponseWriter { return response.ResponseWriter }

func (response *responseLog) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffer, err := http.NewResponseController(response.ResponseWriter).Hijack()
	if err == nil {
		response.status = http.StatusSwitchingProtocols
	}
	return connection, buffer, err
}

func (response *responseLog) WriteHeader(status int) {
	if status >= 200 && response.status == 0 {
		response.status = status
	}
	response.ResponseWriter.WriteHeader(status)
}

func (response *responseLog) Write(body []byte) (int, error) {
	if response.status == 0 {
		response.WriteHeader(http.StatusOK)
	}
	return response.ResponseWriter.Write(body)
}
