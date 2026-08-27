package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestAddSimulacraCatalog(t *testing.T) {
	openAIModels := make(map[string]any, len(modelAliases))
	for _, alias := range modelAliases {
		openAIModels[alias.Upstream] = map[string]any{
			"id": alias.Upstream, "name": alias.Upstream, "release_date": "2026-01-01",
			"attachment": true, "reasoning": true,
			"reasoning_options": []any{map[string]any{"type": "effort", "values": []string{"low", "high"}}},
			"tool_call":         true, "limit": map[string]int{"context": 1000, "output": 100},
			"experimental": map[string]any{"modes": map[string]any{"fast": map[string]any{}}},
		}
	}
	source, err := json.Marshal(map[string]any{
		"openai": map[string]any{"id": "openai", "name": "OpenAI", "env": []string{"OPENAI_API_KEY"}, "npm": "@ai-sdk/openai", "models": openAIModels},
		"other":  map[string]any{"id": "other", "name": "Other", "env": []string{}, "npm": "other", "models": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	body, err := addSimulacraCatalog(source, "https://codex.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		API    string `json:"api"`
		NPM    string `json:"npm"`
		Models map[string]struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			ReasoningOptions []struct {
				Values []string `json:"values"`
			} `json:"reasoning_options"`
			Experimental json.RawMessage `json:"experimental"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog["other"]; !ok {
		t.Fatal("existing provider was removed")
	}
	simulacra := catalog["simulacra"]
	if simulacra.ID != "simulacra" || simulacra.Name != "Simulacra" || simulacra.API != "https://codex.example.test" {
		t.Fatalf("provider = %#v", simulacra)
	}
	if simulacra.NPM != "@ai-sdk/openai" || len(simulacra.Models) != len(modelAliases) {
		t.Fatalf("provider = %#v", simulacra)
	}
	master := simulacra.Models["master"]
	if master.ID != "master" || master.Name != "Master (5.6 Sol)" || len(master.ReasoningOptions[0].Values) != 2 || len(master.Experimental) != 0 {
		t.Fatalf("master = %#v", master)
	}
}

func TestRequestOriginUsesForwardedOrigin(t *testing.T) {
	request := httptest.NewRequest("GET", "http://127.0.0.1/api.json", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "codex.example.test")
	if origin := requestOrigin(request); origin != "https://codex.example.test" {
		t.Fatalf("origin = %q", origin)
	}
}
