package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const defaultUpstream = "https://chatgpt.com/backend-api/codex"

type config struct {
	listen              string
	upstream            *url.URL
	proxyToken          string
	publicURL           string
	oauthCredentialFile string
	oauthStateFile      string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	oauth, err := newOAuthManager(cfg.oauthCredentialFile, cfg.oauthStateFile, http.DefaultClient)
	if err != nil {
		logger.Error("invalid OAuth credential", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		logger.Error("listen failed", "error", err)
		os.Exit(1)
	}
	logger.Info("proxy listening", "address", cfg.listen, "upstream", cfg.upstream.Redacted())
	if err := serve(ctx, listener, newHandler(cfg, oauth, logger), logger, 10*time.Second); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
	refreshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := oauth.wait(refreshCtx); err != nil {
		logger.Error("OAuth shutdown failed", "error", err)
	}
}

func loadConfig() (config, error) {
	upstream, err := url.Parse(env("UPSTREAM_URL", defaultUpstream))
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" {
		return config{}, errors.New("UPSTREAM_URL must be an absolute HTTPS URL")
	}
	if upstream.User != nil || upstream.RawQuery != "" || upstream.ForceQuery || upstream.Fragment != "" {
		return config{}, errors.New("UPSTREAM_URL must not contain user information, a query, or a fragment")
	}
	publicURL, err := url.Parse(os.Getenv("PUBLIC_URL"))
	if err != nil || (publicURL.Scheme != "https" && publicURL.Scheme != "http") || publicURL.Host == "" {
		return config{}, errors.New("PUBLIC_URL must be an absolute HTTP or HTTPS URL")
	}
	if publicURL.User != nil || publicURL.RawQuery != "" || publicURL.ForceQuery || publicURL.Fragment != "" || (publicURL.Path != "" && publicURL.Path != "/") {
		return config{}, errors.New("PUBLIC_URL must contain only a scheme and host")
	}
	allowUnauthenticated := os.Getenv("ALLOW_UNAUTHENTICATED")
	if allowUnauthenticated != "" && allowUnauthenticated != "true" && allowUnauthenticated != "false" {
		return config{}, errors.New("ALLOW_UNAUTHENTICATED must be true or false")
	}
	proxyToken, err := loadProxyToken()
	if err != nil {
		return config{}, err
	}
	if proxyToken == "" && allowUnauthenticated != "true" {
		return config{}, errors.New("a proxy token is required unless ALLOW_UNAUTHENTICATED=true")
	}
	oauthCredentialFile := os.Getenv("OAUTH_CREDENTIAL_FILE")
	if oauthCredentialFile == "" {
		return config{}, errors.New("OAUTH_CREDENTIAL_FILE is required")
	}
	oauthStateFile := os.Getenv("OAUTH_STATE_FILE")
	if oauthStateFile == "" {
		return config{}, errors.New("OAUTH_STATE_FILE is required")
	}
	return config{
		listen:              env("LISTEN_ADDR", "127.0.0.1:8080"),
		publicURL:           strings.TrimRight(publicURL.String(), "/"),
		upstream:            upstream,
		proxyToken:          proxyToken,
		oauthCredentialFile: oauthCredentialFile,
		oauthStateFile:      oauthStateFile,
	}, nil
}

func loadProxyToken() (string, error) {
	token := os.Getenv("PROXY_TOKEN")
	tokenFile := os.Getenv("PROXY_TOKEN_FILE")
	if token != "" && tokenFile != "" {
		return "", errors.New("PROXY_TOKEN and PROXY_TOKEN_FILE must not both be set")
	}
	if tokenFile == "" {
		return token, nil
	}

	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", errors.New("cannot read PROXY_TOKEN_FILE")
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("PROXY_TOKEN_FILE must not be empty")
	}
	return token, nil
}

func newHandler(cfg config, oauth *oauthManager, logger *slog.Logger) http.Handler {
	catalog := newModelCatalog(cfg.publicURL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.Header.Del("Proxy-Authorization")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.SetURL(cfg.upstream)
			if request.In.URL.Path == "/models" {
				request.Out.Header.Del("Accept-Encoding")
				request.Out.Header.Del("If-None-Match")
			}
			request.SetXForwarded()
			request.Out.Host = cfg.upstream.Host
		},
		FlushInterval:  -1,
		ModifyResponse: rewriteModelsResponse,
		ErrorHandler: func(response http.ResponseWriter, request *http.Request, err error) {
			logger.Error("upstream request failed", "method", request.Method, "path", request.URL.Path)
			status := http.StatusBadGateway
			var sizeError *http.MaxBytesError
			if errors.As(err, &sizeError) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(response, http.StatusText(status), status)
		},
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		response := &responseLog{ResponseWriter: writer}
		started := time.Now()
		defer func() {
			logger.Info("request completed", "method", request.Method, "path", request.URL.Path, "status", response.status, "duration_ms", time.Since(started).Milliseconds())
		}()
		if request.URL.Path == "/healthz" {
			if request.Method != http.MethodGet {
				response.Header().Set("Allow", "GET")
				http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			response.Header().Set("Cache-Control", "no-store")
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok", "service": "codex-proxy"})
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/api.json" {
			if err := catalog.serve(response, request); err != nil {
				logger.Error("model catalog unavailable", "error", err)
				http.Error(response, "model catalog unavailable", http.StatusBadGateway)
			}
			return
		}
		if cfg.proxyToken != "" && !validProxyToken(request.Header.Get("Proxy-Authorization"), cfg.proxyToken) {
			response.Header().Set("WWW-Authenticate", `Bearer realm="codex-proxy"`)
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.URL.Path == "/readyz" {
			if request.Method != http.MethodGet {
				response.Header().Set("Allow", "GET")
				http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			serveReadiness(response, request, oauth, cfg.upstream)
			return
		}
		if request.ContentLength > maxRequestBytes {
			http.Error(response, "request body exceeds size limit", http.StatusRequestEntityTooLarge)
			return
		}
		if request.Body != nil {
			request.Body = http.MaxBytesReader(response, request.Body, maxRequestBytes)
		}
		controller := http.NewResponseController(response)
		_ = controller.SetReadDeadline(time.Now().Add(60 * time.Second))
		if err := rewriteRequestModel(request); err != nil {
			status := http.StatusBadRequest
			var sizeError *http.MaxBytesError
			if errors.As(err, &sizeError) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(response, http.StatusText(status), status)
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/responses" || request.Header.Get("Upgrade") != "" {
			_ = controller.SetReadDeadline(time.Time{})
		}
		credential, err := oauth.current(request.Context())
		if err != nil {
			logger.Error("OAuth credential unavailable", "error", err)
			status := http.StatusBadGateway
			var refreshError *oauthRefreshError
			if errors.As(err, &refreshError) && refreshError.StatusCode == http.StatusUnauthorized {
				status = http.StatusUnauthorized
			}
			http.Error(response, "upstream authentication unavailable", status)
			return
		}
		sessionID, err := newSessionID()
		if err != nil {
			logger.Error("session ID generation failed", "error", err)
			http.Error(response, "internal error", http.StatusInternalServerError)
			return
		}
		request.Header.Set("Authorization", "Bearer "+credential.Access)
		request.Header.Set("ChatGPT-Account-ID", credential.AccountID)
		request.Header.Set("Originator", "opencode")
		request.Header.Set("Session-ID", sessionID)
		logger.Info("proxying request", "method", request.Method, "path", request.URL.Path)
		proxy.ServeHTTP(response, request)
	})
}

func serveReadiness(response http.ResponseWriter, request *http.Request, oauth *oauthManager, upstream *url.URL) {
	type check struct {
		Status    string `json:"status"`
		ExpiresAt string `json:"expires_at,omitempty"`
	}
	body := struct {
		Status   string           `json:"status"`
		Service  string           `json:"service"`
		Upstream string           `json:"upstream"`
		Checks   map[string]check `json:"checks"`
	}{
		Status:   "ok",
		Service:  "codex-proxy",
		Upstream: upstream.Redacted(),
		Checks:   make(map[string]check, 1),
	}

	status := http.StatusOK
	credential, err := oauth.current(request.Context())
	if err != nil {
		body.Status = "unhealthy"
		oauthStatus := "unavailable"
		var refreshError *oauthRefreshError
		if errors.As(err, &refreshError) && refreshError.StatusCode == http.StatusUnauthorized {
			oauthStatus = "unauthorized"
		}
		body.Checks["oauth"] = check{Status: oauthStatus}
		status = http.StatusServiceUnavailable
	} else {
		body.Checks["oauth"] = check{
			Status:    "ready",
			ExpiresAt: time.UnixMilli(credential.Expires).UTC().Format(time.RFC3339),
		}
	}

	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func validProxyToken(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimPrefix(header, prefix)
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
