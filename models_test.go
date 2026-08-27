package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewriteRequestModel(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"master","input":"hello"}`))
	if err := rewriteRequestModel(request); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if value.Model != "gpt-5.6-sol" || value.Input != "hello" {
		t.Fatalf("body = %s", body)
	}
}

func TestRewriteModelsResponse(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/models", nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Request:    request,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"models":[
			{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","supported_reasoning_levels":[{"effort":"max"}]},
			{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra","supported_reasoning_levels":[{"effort":"high"}]},
			{"slug":"gpt-reserve","display_name":"GPT Reserve"}
		]}`)),
	}
	if err := rewriteModelsResponse(response); err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []struct {
			Slug                     string `json:"slug"`
			DisplayName              string `json:"display_name"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("models = %#v", catalog.Models)
	}
	if catalog.Models[0].Slug != "master" || catalog.Models[0].DisplayName != "Master" || catalog.Models[0].SupportedReasoningLevels[0].Effort != "max" {
		t.Fatalf("master = %#v", catalog.Models[0])
	}
	if catalog.Models[1].Slug != "marshal" || catalog.Models[1].DisplayName != "Marshal" {
		t.Fatalf("marshal = %#v", catalog.Models[1])
	}
}
