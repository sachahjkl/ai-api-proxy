package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestOAuthManagerRefreshesAndPersistsCredential(t *testing.T) {
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error(err)
			http.Error(response, "invalid form", http.StatusBadRequest)
			return
		}
		received = request.PostForm
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600,
		})
	}))
	defer server.Close()

	directory := t.TempDir()
	seedFile := directory + "/seed.json"
	stateFile := directory + "/state/oauth.json"
	seed := oauthCredential{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(-time.Hour).UnixMilli(), AccountID: "account"}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := newOAuthManager(seedFile, stateFile, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	manager.tokenURL = server.URL

	credential, err := manager.current(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if credential.Access != "new-access" || credential.Refresh != "new-refresh" || credential.AccountID != "account" {
		t.Fatalf("credential = %#v", credential)
	}
	if received.Get("grant_type") != "refresh_token" || received.Get("refresh_token") != "old-refresh" || received.Get("client_id") != oauthClientID {
		t.Fatalf("refresh form = %#v", received)
	}
	persisted, err := readOAuthCredential(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Access != "new-access" || persisted.Refresh != "new-refresh" {
		t.Fatalf("persisted credential = %#v", persisted)
	}
}
