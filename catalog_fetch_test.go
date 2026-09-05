package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogConcurrentFetchAndCancellation(t *testing.T) {
	fixture := catalogFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	var released sync.Once
	defer released.Do(func() { close(release) })
	catalog := newModelCatalog("https://proxy.test")
	catalog.sourceURL = server.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := catalog.current(ctx); result <- err }()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled catalog request waited for fetch")
	}
	released.Do(func() { close(release) })
	if _, err := catalog.current(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch calls=%d", calls.Load())
	}
}

func TestCatalogRejectsFetchFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"status", http.StatusServiceUnavailable, "unavailable"},
		{"oversized", http.StatusOK, strings.Repeat("x", maxCatalogBytes+1)},
		{"malformed", http.StatusOK, "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			catalog := newModelCatalog("https://proxy.test")
			catalog.sourceURL = server.URL
			if _, err := catalog.current(t.Context()); err == nil {
				t.Fatal("accepted invalid response")
			}
			if len(catalog.body) != 0 {
				t.Fatal("cached invalid response")
			}
		})
	}
}
