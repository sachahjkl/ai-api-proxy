package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	publicURL string
	pending   chan struct{}
	lastError error
	retryAt   time.Time
}

func newModelCatalog(publicURL string) *modelCatalog {
	return &modelCatalog{
		client:    &http.Client{Timeout: 10 * time.Second},
		sourceURL: defaultModelCatalogURL,
		publicURL: publicURL,
	}
}

func (catalog *modelCatalog) serve(response http.ResponseWriter, request *http.Request) error {
	body, err := catalog.current(request.Context())
	if err != nil {
		return err
	}
	response.Header().Set("Cache-Control", "public, max-age=300")
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(body)
	return nil
}

func (catalog *modelCatalog) current(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		catalog.mu.Lock()
		if len(catalog.body) != 0 && time.Now().Before(catalog.expires) {
			body := catalog.body
			catalog.mu.Unlock()
			return body, nil
		}
		if catalog.lastError != nil && time.Now().Before(catalog.retryAt) {
			err := catalog.lastError
			catalog.mu.Unlock()
			return nil, err
		}
		pending := catalog.pending
		if pending == nil {
			pending = make(chan struct{})
			catalog.pending = pending
			go catalog.update()
		}
		catalog.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending:
		}
	}
}

func (catalog *modelCatalog) update() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, err := catalog.fetch(ctx)
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	catalog.lastError = err
	if err != nil {
		catalog.retryAt = time.Now().Add(30 * time.Second)
	} else {
		catalog.body = body
		catalog.expires = time.Now().Add(5 * time.Minute)
	}
	close(catalog.pending)
	catalog.pending = nil
}

func (catalog *modelCatalog) fetch(ctx context.Context) ([]byte, error) {
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
		return nil, fmt.Errorf("OpenCode model catalog returned status %d", upstream.StatusCode)
	}
	body, err := readLimited(upstream.Body, maxCatalogBytes)
	if err != nil {
		return nil, errors.New("cannot read OpenCode model catalog or catalog exceeds size limit")
	}
	return addSimulacraCatalog(body, catalog.publicURL)
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
		if err := json.Unmarshal(source, &model); err != nil || model == nil {
			return nil, fmt.Errorf("OpenAI model %s must be an object", alias.Upstream)
		}
		model["id"], _ = json.Marshal(id)
		model["name"], _ = json.Marshal(alias.Name)
		var limit map[string]json.RawMessage
		if err := json.Unmarshal(model["limit"], &limit); err != nil || limit == nil {
			return nil, fmt.Errorf("OpenAI model %s limits must be an object", alias.Upstream)
		}
		for _, name := range []string{"context", "input", "output"} {
			var value int64
			if err := json.Unmarshal(limit[name], &value); err != nil || value <= 0 {
				return nil, fmt.Errorf("OpenAI model %s has an invalid %s limit", alias.Upstream, name)
			}
		}
		if !alias.NativeLimits {
			if alias.LongContext {
				limit["context"], _ = json.Marshal(1_000_000)
				limit["input"], _ = json.Marshal(872_000)
			} else {
				limit["context"], _ = json.Marshal(400_000)
				limit["input"], _ = json.Marshal(272_000)
			}
		}
		model["limit"], _ = json.Marshal(limit)
		delete(model, "experimental")
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
