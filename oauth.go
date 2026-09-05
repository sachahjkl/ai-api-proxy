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

type oauthRefreshError struct {
	StatusCode int
}

func (err *oauthRefreshError) Error() string {
	return fmt.Sprintf("OAuth refresh failed with status %d", err.StatusCode)
}

type oauthManager struct {
	mu         sync.Mutex
	credential oauthCredential
	stateFile  string
	client     *http.Client
	tokenURL   string
	pending    chan struct{}
	dirty      bool
	lastError  error
	retryAt    time.Time
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
	for {
		if err := ctx.Err(); err != nil {
			return oauthCredential{}, err
		}
		manager.mu.Lock()
		pending := manager.pending
		if pending == nil {
			if manager.lastError != nil && time.Now().Before(manager.retryAt) {
				err := manager.lastError
				manager.mu.Unlock()
				return oauthCredential{}, err
			}
			if !manager.dirty && manager.credential.Expires > time.Now().Add(time.Minute).UnixMilli() {
				credential := manager.credential
				manager.mu.Unlock()
				return credential, nil
			}
			pending = make(chan struct{})
			manager.pending = pending
			go manager.update(manager.credential, manager.dirty)
		}
		manager.mu.Unlock()
		select {
		case <-ctx.Done():
			return oauthCredential{}, ctx.Err()
		case <-pending:
		}
	}
}

// Finish token rotation even if the initiating client disconnects.
func (manager *oauthManager) update(credential oauthCredential, dirty bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	if !dirty {
		credential, err = manager.refresh(ctx, credential)
		dirty = err == nil
	}
	if err == nil {
		err = writeOAuthCredential(manager.stateFile, credential)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if dirty {
		manager.credential = credential
	}
	manager.dirty = dirty && err != nil
	manager.lastError = err
	if err != nil {
		manager.retryAt = time.Now().Add(30 * time.Second)
	}
	close(manager.pending)
	manager.pending = nil
}

func (manager *oauthManager) wait(ctx context.Context) error {
	manager.mu.Lock()
	pending := manager.pending
	manager.mu.Unlock()
	if pending == nil {
		return nil
	}
	select {
	case <-pending:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (manager *oauthManager) refresh(ctx context.Context, credential oauthCredential) (oauthCredential, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {credential.Refresh},
		"client_id":     {oauthClientID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, manager.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthCredential{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "codex-proxy")
	response, err := manager.client.Do(request)
	if err != nil {
		return oauthCredential{}, errors.New("OAuth refresh request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return oauthCredential{}, &oauthRefreshError{StatusCode: response.StatusCode}
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	body, err := readLimited(response.Body, 1<<20)
	if err != nil || json.Unmarshal(body, &result) != nil {
		return oauthCredential{}, errors.New("OAuth refresh returned invalid JSON or exceeded the size limit")
	}
	if result.ExpiresIn <= 60 || result.ExpiresIn > 365*24*60*60 {
		return oauthCredential{}, errors.New("OAuth refresh returned an invalid lifetime")
	}
	next := oauthCredential{
		Access:    result.AccessToken,
		Refresh:   result.RefreshToken,
		Expires:   time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UnixMilli(),
		AccountID: credential.AccountID,
	}
	if err := next.validate(); err != nil {
		return oauthCredential{}, fmt.Errorf("OAuth refresh returned an invalid credential: %w", err)
	}
	return next, nil
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
	if err := decoder.Decode(new(any)); err != io.EOF {
		return oauthCredential{}, errors.New("OAuth credential file contains trailing data")
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
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync OAuth state file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close OAuth state file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace OAuth state file: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open OAuth state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync OAuth state directory: %w", err)
	}
	return nil
}
