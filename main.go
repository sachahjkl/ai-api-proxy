package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
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

	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           newHandler(cfg, oauth, logger),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
	}()

	logger.Info("proxy listening", "address", cfg.listen, "upstream", cfg.upstream.Redacted())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	upstream, err := url.Parse(env("UPSTREAM_URL", defaultUpstream))
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" {
		return config{}, errors.New("UPSTREAM_URL must be an absolute HTTPS URL")
	}
	if upstream.RawQuery != "" || upstream.Fragment != "" {
		return config{}, errors.New("UPSTREAM_URL must not contain a query or fragment")
	}
	proxyToken, err := loadProxyToken()
	if err != nil {
		return config{}, err
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
		listen:              env("LISTEN_ADDR", ":8080"),
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
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.Header.Del("Proxy-Authorization")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.SetURL(cfg.upstream)
			request.SetXForwarded()
			request.Out.Host = cfg.upstream.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(response http.ResponseWriter, request *http.Request, err error) {
			logger.Error("upstream request failed", "method", request.Method, "path", request.URL.Path, "error", err)
			http.Error(response, "upstream unavailable", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			response.Header().Set("Content-Type", "text/plain; charset=utf-8")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte("ok\n"))
			return
		}
		if cfg.proxyToken != "" && !validProxyToken(request.Header.Get("Proxy-Authorization"), cfg.proxyToken) {
			response.Header().Set("WWW-Authenticate", `Bearer realm="codex-proxy"`)
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		credential, err := oauth.current(request.Context())
		if err != nil {
			logger.Error("OAuth credential unavailable", "error", err)
			http.Error(response, "upstream authentication unavailable", http.StatusBadGateway)
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
