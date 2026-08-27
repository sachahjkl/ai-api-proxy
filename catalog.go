package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultModelCatalogURL = "https://models.opencode.ai/api.json"

type modelCatalog struct {
	mu        sync.Mutex
	client    *http.Client
	sourceURL string
	body      []byte
	expires   time.Time
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{
		client:    &http.Client{Timeout: 10 * time.Second},
		sourceURL: defaultModelCatalogURL,
	}
}

func (catalog *modelCatalog) serve(response http.ResponseWriter, request *http.Request) error {
	source, err := catalog.current(request.Context())
	if err != nil {
		return err
	}
	body, err := addSimulacraCatalog(source, requestOrigin(request))
	if err != nil {
		return err
	}
	response.Header().Set("Cache-Control", "public, max-age=300")
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_, err = response.Write(body)
	return err
}

func (catalog *modelCatalog) current(ctx context.Context) ([]byte, error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if len(catalog.body) != 0 && time.Now().Before(catalog.expires) {
		return catalog.body, nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, catalog.sourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "codex-proxy")
	upstream, err := catalog.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenCode model catalog: %w", err)
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, upstream.Body)
		return nil, fmt.Errorf("OpenCode model catalog returned status %d", upstream.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(upstream.Body, 16<<20))
	if err != nil {
		return nil, errors.New("cannot read OpenCode model catalog")
	}
	catalog.body = body
	catalog.expires = time.Now().Add(5 * time.Minute)
	return catalog.body, nil
}

func requestOrigin(request *http.Request) string {
	scheme := strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0])
	if scheme == "" {
		scheme = "http"
		if request.TLS != nil {
			scheme = "https"
		}
	}
	host := strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = request.Host
	}
	return (&url.URL{Scheme: scheme, Host: host}).String()
}

func addSimulacraCatalog(body []byte, publicURL string) ([]byte, error) {
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, errors.New("OpenCode model catalog contains invalid JSON")
	}
	var openAI struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(providers["openai"], &openAI); err != nil {
		return nil, errors.New("OpenCode model catalog has no OpenAI provider")
	}

	models := make(map[string]json.RawMessage, len(modelAliases))
	for id, alias := range modelAliases {
		source, ok := openAI.Models[alias.Upstream]
		if !ok {
			continue
		}
		var model map[string]json.RawMessage
		if err := json.Unmarshal(source, &model); err != nil {
			return nil, fmt.Errorf("decode OpenAI model %s: %w", alias.Upstream, err)
		}
		model["id"], _ = json.Marshal(id)
		model["name"], _ = json.Marshal(alias.Name)
		delete(model, "provider")
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, fmt.Errorf("encode Simulacra model %s: %w", id, err)
		}
		models[id] = encoded
	}
	if len(models) != len(modelAliases) {
		return nil, errors.New("OpenCode model catalog does not contain every mapped OpenAI model")
	}

	provider, err := json.Marshal(map[string]any{
		"id":     "simulacra",
		"name":   "Simulacra",
		"env":    []string{},
		"npm":    "@ai-sdk/openai",
		"api":    publicURL,
		"models": models,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Simulacra provider: %w", err)
	}
	providers["simulacra"] = provider
	body, err = json.Marshal(providers)
	if err != nil {
		return nil, fmt.Errorf("encode OpenCode model catalog: %w", err)
	}
	return body, nil
}
