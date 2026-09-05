package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadProxyTokenFromFile(t *testing.T) {
	t.Setenv("PROXY_TOKEN", "")
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("gateway-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_TOKEN_FILE", tokenFile)

	token, err := loadProxyToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != "gateway-secret" {
		t.Fatalf("token = %q", token)
	}
}

func TestProxyForwardsCodexRequest(t *testing.T) {
	t.Helper()
	var received *http.Request
	var body string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		received = request.Clone(request.Context())
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		body = string(data)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: done\n\n"))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	oauth := &oauthManager{credential: oauthCredential{
		Access: "server-oauth-token", Refresh: "refresh", Expires: time.Now().Add(time.Hour).UnixMilli(), AccountID: "server-account",
	}}
	handler := newHandler(config{upstream: upstreamURL, proxyToken: "gateway-secret"}, oauth, slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodPost, proxy.URL+"/responses?foo=bar", strings.NewReader(`{"model":"captain"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer chatgpt-oauth-token")
	request.Header.Set("Proxy-Authorization", "Bearer gateway-secret")
	request.Header.Set("ChatGPT-Account-ID", "acct_123")
	request.Header.Set("X-Forwarded-For", "spoofed")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if received == nil {
		t.Fatal("upstream did not receive request")
	}
	if received.URL.Path != "/backend-api/codex/responses" || received.URL.RawQuery != "foo=bar" {
		t.Errorf("upstream URL = %s", received.URL.String())
	}
	if received.Header.Get("Authorization") != "Bearer server-oauth-token" {
		t.Errorf("Authorization = %q", received.Header.Get("Authorization"))
	}
	if received.Header.Get("Proxy-Authorization") != "" {
		t.Error("Proxy-Authorization leaked upstream")
	}
	if strings.Contains(received.Header.Get("X-Forwarded-For"), "spoofed") {
		t.Errorf("spoofed X-Forwarded-For was preserved: %q", received.Header.Get("X-Forwarded-For"))
	}
	if received.Header.Get("ChatGPT-Account-ID") != "server-account" {
		t.Errorf("ChatGPT-Account-ID = %q", received.Header.Get("ChatGPT-Account-ID"))
	}
	if received.Header.Get("Originator") != "opencode" || received.Header.Get("Session-ID") == "" {
		t.Errorf("missing server context headers")
	}
	var receivedBody struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &receivedBody); err != nil {
		t.Fatal(err)
	}
	if receivedBody.Model != "gpt-5.4" {
		t.Errorf("model = %q", receivedBody.Model)
	}
}

func TestProxyStreamsCodexResponse(t *testing.T) {
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: first\n\n"))
		response.(http.Flusher).Flush()
		<-releaseUpstream
		_, _ = response.Write([]byte("data: second\n\n"))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	oauth := &oauthManager{credential: oauthCredential{
		Access: "server-oauth-token", Refresh: "refresh", Expires: time.Now().Add(time.Hour).UnixMilli(), AccountID: "server-account",
	}}
	proxy := httptest.NewServer(newHandler(config{upstream: upstreamURL}, oauth, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer proxy.Close()

	responseChannel := make(chan *http.Response, 1)
	errorChannel := make(chan error, 1)
	go func() {
		response, err := http.Post(proxy.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true}`))
		if err != nil {
			errorChannel <- err
			return
		}
		responseChannel <- response
	}()

	var response *http.Response
	select {
	case response = <-responseChannel:
	case err := <-errorChannel:
		close(releaseUpstream)
		t.Fatal(err)
	case <-time.After(time.Second):
		close(releaseUpstream)
		t.Fatal("proxy buffered the first SSE event")
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	firstEvent, err := reader.ReadString('\n')
	if err != nil {
		close(releaseUpstream)
		t.Fatal(err)
	}
	if firstEvent != "data: first\n" {
		close(releaseUpstream)
		t.Fatalf("first event = %q", firstEvent)
	}

	close(releaseUpstream)
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(remainder) != "\ndata: second\n\n" {
		t.Fatalf("remaining stream = %q", remainder)
	}
}

func TestProxyAuthentication(t *testing.T) {
	upstream, err := url.Parse(defaultUpstream)
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandler(config{upstream: upstream, proxyToken: "gateway-secret"}, &oauthManager{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestHealthReportsReadyOAuthWithoutProxyAuthentication(t *testing.T) {
	upstream, err := url.Parse(defaultUpstream)
	if err != nil {
		t.Fatal(err)
	}
	oauth := &oauthManager{credential: oauthCredential{
		Access: "access", Refresh: "refresh", Expires: time.Now().Add(time.Hour).UnixMilli(), AccountID: "account",
	}}
	handler := newHandler(config{upstream: upstream, proxyToken: "gateway-secret"}, oauth, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	var health struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status    string `json:"status"`
			ExpiresAt string `json:"expires_at"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.Checks["oauth"].Status != "ready" || health.Checks["oauth"].ExpiresAt == "" {
		t.Fatalf("health = %#v", health)
	}
}

func TestOAuthUnauthorizedStatusIsPreserved(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
	}))
	defer tokenServer.Close()

	upstream, err := url.Parse(defaultUpstream)
	if err != nil {
		t.Fatal(err)
	}
	oauth := &oauthManager{
		credential: oauthCredential{Access: "expired", Refresh: "revoked", Expires: time.Now().Add(-time.Hour).UnixMilli(), AccountID: "account"},
		client:     tokenServer.Client(),
		tokenURL:   tokenServer.URL,
	}
	handler := newHandler(config{upstream: upstream}, oauth, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"master"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, body = %q", healthResponse.Code, healthResponse.Body.String())
	}
	var health struct {
		Checks map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(healthResponse.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Checks["oauth"].Status != "unauthorized" {
		t.Fatalf("health = %#v", health)
	}
}
