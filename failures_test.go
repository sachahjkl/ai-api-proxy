package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func expiredCredential() oauthCredential {
	return oauthCredential{Access: "old", Refresh: "old-refresh", AccountID: "account", Expires: time.Now().Add(-time.Hour).UnixMilli()}
}

func writeRefresh(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(response, `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)
}

func TestOAuthConcurrentRefreshAndCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		writeRefresh(w)
	}))
	defer server.Close()
	manager := &oauthManager{credential: expiredCredential(), stateFile: filepath.Join(t.TempDir(), "oauth.json"), client: server.Client(), tokenURL: server.URL}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, err := manager.current(ctx); result <- err }()
	<-started
	cancel()
	if err := manager.wait(ctx); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("wait ignored cancellation: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("canceled request waited for refresh")
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			credential, err := manager.current(t.Context())
			if err != nil || credential.Refresh != "rotated" {
				t.Errorf("credential=%v error=%v", credential, err)
			}
		}()
	}
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", calls.Load())
	}
	if err := manager.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthPersistenceRetryKeepsRotatedToken(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeRefresh(w) }))
	defer server.Close()
	directory := t.TempDir()
	blocked := filepath.Join(directory, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := &oauthManager{credential: expiredCredential(), stateFile: filepath.Join(blocked, "oauth.json"), client: server.Client(), tokenURL: server.URL}
	if _, err := manager.current(t.Context()); err == nil {
		t.Fatal("expected persistence failure")
	}
	if manager.credential.Refresh != "rotated" || !manager.dirty {
		t.Fatal("rotated credential was discarded")
	}
	if _, err := manager.current(t.Context()); err == nil {
		t.Fatal("expected retry delay")
	}
	manager.stateFile = filepath.Join(directory, "oauth.json")
	manager.retryAt = time.Time{}
	credential, err := manager.current(t.Context())
	if err != nil || credential.Refresh != "rotated" {
		t.Fatalf("credential=%v error=%v", credential, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh repeated after rotation: %d", calls.Load())
	}
	info, err := os.Stat(manager.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	restarted, err := newOAuthManager("missing-seed", manager.stateFile, server.Client())
	if err != nil || restarted.credential.Refresh != "rotated" {
		t.Fatalf("restart error=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOAuthRefreshHasDeadline(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("OAuth refresh has no bounded deadline")
		}
		return nil, context.DeadlineExceeded
	})}
	manager := &oauthManager{credential: expiredCredential(), client: client, tokenURL: oauthTokenURL}
	if _, err := manager.current(t.Context()); err == nil {
		t.Fatal("accepted failed refresh")
	}
}

func TestOAuthRejectsInvalidRefresh(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"access_token":"a","refresh_token":"r","expires_in":0}`,
		`{"access_token":"a","refresh_token":"r","expires_in":-1}`,
		`{"access_token":"a","refresh_token":"r","expires_in":9223372036854775807}`,
		`{"access_token":"a","refresh_token":"r","expires_in":3600} {}`,
		strings.Repeat("x", (1<<20)+1),
	} {
		t.Run(body[:min(len(body), 80)], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, body) }))
			defer server.Close()
			manager := &oauthManager{credential: expiredCredential(), stateFile: filepath.Join(t.TempDir(), "oauth.json"), client: server.Client(), tokenURL: server.URL}
			for range 2 {
				if _, err := manager.current(t.Context()); err == nil {
					t.Fatal("accepted invalid refresh")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("failed refresh was not throttled")
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	defaults := map[string]string{
		"PUBLIC_URL": "https://proxy.test", "PROXY_TOKEN": "secret", "PROXY_TOKEN_FILE": "",
		"OAUTH_CREDENTIAL_FILE": "/seed", "OAUTH_STATE_FILE": "/state", "UPSTREAM_URL": "", "LISTEN_ADDR": "", "ALLOW_UNAUTHENTICATED": "",
	}
	for key, value := range defaults {
		t.Setenv(key, value)
	}
	cfg, err := loadConfig()
	if err != nil || cfg.listen != "127.0.0.1:8080" || cfg.publicURL != "https://proxy.test" {
		t.Fatalf("config=%v error=%v", cfg, err)
	}
	for _, test := range []struct{ key, value string }{
		{"PUBLIC_URL", ""}, {"PUBLIC_URL", "https://user:pass@proxy.test"}, {"PUBLIC_URL", "https://proxy.test/path"},
		{"PUBLIC_URL", "https://proxy.test?"}, {"PUBLIC_URL", "ftp://proxy.test"}, {"PUBLIC_URL", "https://proxy.test#fragment"},
		{"UPSTREAM_URL", "http://upstream.test"}, {"UPSTREAM_URL", "https://user:pass@upstream.test"}, {"UPSTREAM_URL", "https://upstream.test?query"},
		{"PROXY_TOKEN", ""}, {"ALLOW_UNAUTHENTICATED", "yes"}, {"OAUTH_CREDENTIAL_FILE", ""}, {"OAUTH_STATE_FILE", ""},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := loadConfig(); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	t.Setenv("PROXY_TOKEN", "")
	t.Setenv("ALLOW_UNAUTHENTICATED", "true")
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestHealthIsLocalAndReadinessRequiresAuthentication(t *testing.T) {
	upstream, _ := url.Parse(defaultUpstream)
	handler := newHandler(config{upstream: upstream, proxyToken: "secret"}, &oauthManager{}, testLogger())
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/healthz", 200}, {"POST", "/healthz", 405}, {"GET", "/readyz", 401},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.status {
			t.Fatalf("%s %s: %d", test.method, test.path, response.Code)
		}
	}
}

func TestProxyRejectsOversizedRequests(t *testing.T) {
	upstream, _ := url.Parse(defaultUpstream)
	handler := newHandler(config{upstream: upstream}, &oauthManager{}, testLogger())
	for _, length := range []int64{-1, maxRequestBytes + 1} {
		request := httptest.NewRequest("POST", "/responses", strings.NewReader(strings.Repeat("x", maxRequestBytes+1)))
		request.ContentLength = length
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status=%d", response.Code)
		}
	}
}

func TestCatalogFetchCacheAndValidation(t *testing.T) {
	fixture := catalogFixture(t)
	var calls atomic.Int32
	var valid atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if valid.Load() {
			_, _ = w.Write(fixture)
		} else {
			_, _ = io.WriteString(w, `{"openai":{"models":null}}`)
		}
	}))
	defer server.Close()
	catalog := newModelCatalog("https://proxy.test")
	catalog.sourceURL = server.URL
	for range 2 {
		if _, err := catalog.current(t.Context()); err == nil {
			t.Fatal("accepted invalid catalog")
		}
	}
	if calls.Load() != 1 || len(catalog.body) != 0 {
		t.Fatal("invalid catalog cached or failure not throttled")
	}
	valid.Store(true)
	catalog.retryAt = time.Time{}
	for range 2 {
		if _, err := catalog.current(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("fetch calls=%d", calls.Load())
	}
}

func TestCatalogRejectsNullModelAndLimits(t *testing.T) {
	for _, model := range []string{`null`, `{"limit":null}`, `{"limit":{}}`, `{"limit":{"context":1,"input":1,"output":0}}`} {
		source, _ := json.Marshal(map[string]any{"openai": map[string]any{"models": map[string]json.RawMessage{"gpt-6-astra": json.RawMessage(model)}}})
		if _, err := addSimulacraCatalog(source, "https://proxy.test"); err == nil {
			t.Fatal("accepted invalid model")
		}
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := readLimited(strings.NewReader("1234"), 3); err == nil {
		t.Fatal("accepted oversized body")
	}
	if body, err := readLimited(strings.NewReader("123"), 3); err != nil || string(body) != "123" {
		t.Fatalf("body=%s error=%v", body, err)
	}
}

func FuzzCatalog(f *testing.F) {
	f.Add(catalogFixture(f))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"openai":{"models":{"gpt-6-astra":{"limit":null}}}}`))
	f.Fuzz(func(t *testing.T, body []byte) { _, _ = addSimulacraCatalog(body, "https://proxy.test") })
}

func FuzzRequestModel(f *testing.F) {
	f.Add([]byte(`{"model":"master","input":"hello"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		request := httptest.NewRequest("POST", "/responses", strings.NewReader(string(body)))
		_ = rewriteRequestModel(request)
	})
}
