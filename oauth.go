package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthTokenURL = "https://auth.openai.com/oauth/token"
)

type oauthCredential struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"account_id"`
}

type oauthManager struct {
	mu         sync.Mutex
	credential oauthCredential
	stateFile  string
	client     *http.Client
	tokenURL   string
}

func newOAuthManager(seedFile, stateFile string, client *http.Client) (*oauthManager, error) {
	credential, err := readOAuthCredential(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		credential, err = readOAuthCredential(seedFile)
	}
	if err != nil {
		return nil, err
	}
	if err := credential.validate(); err != nil {
		return nil, err
	}
	return &oauthManager{
		credential: credential,
		stateFile:  stateFile,
		client:     client,
		tokenURL:   oauthTokenURL,
	}, nil
}

func (manager *oauthManager) current(ctx context.Context) (oauthCredential, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	if manager.credential.Expires > time.Now().Add(time.Minute).UnixMilli() {
		return manager.credential, nil
	}
	if err := manager.refresh(ctx); err != nil {
		return oauthCredential{}, err
	}
	return manager.credential, nil
}

func (manager *oauthManager) refresh(ctx context.Context) error {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {manager.credential.Refresh},
		"client_id":     {oauthClientID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, manager.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "codex-proxy")
	response, err := manager.client.Do(request)
	if err != nil {
		return fmt.Errorf("OAuth refresh request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, response.Body)
		return fmt.Errorf("OAuth refresh failed with status %d", response.StatusCode)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return errors.New("OAuth refresh returned invalid JSON")
	}
	next := oauthCredential{
		Access:    result.AccessToken,
		Refresh:   result.RefreshToken,
		Expires:   time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UnixMilli(),
		AccountID: manager.credential.AccountID,
	}
	if err := next.validate(); err != nil {
		return fmt.Errorf("OAuth refresh returned an invalid credential: %w", err)
	}
	if err := writeOAuthCredential(manager.stateFile, next); err != nil {
		return err
	}
	manager.credential = next
	return nil
}

func (credential oauthCredential) validate() error {
	if credential.Access == "" || credential.Refresh == "" || credential.Expires <= 0 || credential.AccountID == "" {
		return errors.New("OAuth credential requires access, refresh, expires, and account_id")
	}
	return nil
}

func readOAuthCredential(path string) (oauthCredential, error) {
	file, err := os.Open(path)
	if err != nil {
		return oauthCredential{}, err
	}
	defer file.Close()
	var credential oauthCredential
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil {
		return oauthCredential{}, errors.New("OAuth credential file contains invalid JSON")
	}
	return credential, nil
}

func writeOAuthCredential(path string, credential oauthCredential) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create OAuth state directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".oauth-*")
	if err != nil {
		return fmt.Errorf("create OAuth state file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure OAuth state file: %w", err)
	}
	if err := json.NewEncoder(file).Encode(credential); err != nil {
		file.Close()
		return fmt.Errorf("write OAuth state file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close OAuth state file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace OAuth state file: %w", err)
	}
	return nil
}
