package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type modelAlias struct {
	Name     string
	Upstream string
}

var modelAliases = map[string]modelAlias{
	"master":    {Name: "Master (5.6 Sol)", Upstream: "gpt-5.6-sol"},
	"marshal":   {Name: "Marshal (5.6 Terra)", Upstream: "gpt-5.6-terra"},
	"commander": {Name: "Commander (5.6 Luna)", Upstream: "gpt-5.6-luna"},
	"general":   {Name: "General (5.5)", Upstream: "gpt-5.5"},
	"captain":   {Name: "Captain (5.4)", Upstream: "gpt-5.4"},
	"scout":     {Name: "Scout (5.4 Mini)", Upstream: "gpt-5.4-mini"},
}

func rewriteRequestModel(request *http.Request) error {
	if request.Method != http.MethodPost || request.URL.Path != "/responses" || request.Body == nil {
		return nil
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return errors.New("cannot read request body")
	}
	request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(body))

	var value struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &value); err != nil || value.Model == "" {
		return nil
	}
	alias, ok := modelAliases[value.Model]
	if !ok {
		return nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return errors.New("request body contains invalid JSON")
	}
	fields["model"], _ = json.Marshal(alias.Upstream)
	body, err = json.Marshal(fields)
	if err != nil {
		return errors.New("cannot encode request body")
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func rewriteModelsResponse(response *http.Response) error {
	if response.StatusCode != http.StatusOK || !strings.HasSuffix(response.Request.URL.Path, "/models") {
		return nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return errors.New("cannot read model catalog")
	}
	response.Body.Close()

	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(body, &catalog); err != nil {
		return errors.New("upstream returned an invalid model catalog")
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(catalog["models"], &models); err != nil {
		return errors.New("upstream model catalog has no models")
	}

	byUpstream := make(map[string]struct {
		ID   string
		Name string
	}, len(modelAliases))
	for id, alias := range modelAliases {
		byUpstream[alias.Upstream] = struct {
			ID   string
			Name string
		}{ID: id, Name: alias.Name}
	}

	mapped := make([]map[string]json.RawMessage, 0, len(modelAliases))
	for _, model := range models {
		var slug string
		if err := json.Unmarshal(model["slug"], &slug); err != nil {
			continue
		}
		alias, ok := byUpstream[slug]
		if !ok {
			continue
		}
		model["slug"], _ = json.Marshal(alias.ID)
		model["display_name"], _ = json.Marshal(alias.Name)
		mapped = append(mapped, model)
	}
	catalog["models"], err = json.Marshal(mapped)
	if err != nil {
		return fmt.Errorf("encode mapped models: %w", err)
	}
	body, err = json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("encode model catalog: %w", err)
	}

	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Header.Del("Content-Encoding")
	response.Header.Del("ETag")
	return nil
}
