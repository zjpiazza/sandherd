package codexauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrCredentialUnavailable    = errors.New("credential is unavailable")
	ErrReauthenticationRequired = errors.New("credential reauthentication is required")
)

type SyncClient struct {
	sourceURL string
	authFile  string
	client    *http.Client
	logger    *slog.Logger

	mu          sync.RWMutex
	etag        string
	lastSuccess time.Time
	lastStatus  string
}

func NewSyncClient(sourceURL, authFile string, timeout time.Duration, logger *slog.Logger) (*SyncClient, error) {
	if sourceURL == "" || authFile == "" || !filepath.IsAbs(authFile) || timeout <= 0 {
		return nil, fmt.Errorf("credential source, absolute destination, and positive timeout are required")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SyncClient{sourceURL: sourceURL, authFile: authFile, client: &http.Client{Timeout: timeout}, logger: logger}, nil
}

func (s *SyncClient) Sync(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sourceURL, nil)
	if err != nil {
		return ErrCredentialUnavailable
	}
	s.mu.RLock()
	etag := s.etag
	s.mu.RUnlock()
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := s.client.Do(request)
	if err != nil {
		s.recordStatus(StatusUnavailable, false)
		return ErrCredentialUnavailable
	}
	defer response.Body.Close()
	status := response.Header.Get("X-Sandherd-Credential-Status")
	if status == StatusReauthenticationRequired {
		s.recordStatus(status, false)
		return ErrReauthenticationRequired
	}
	if response.StatusCode == http.StatusNotModified {
		s.recordStatus(StatusReady, true)
		return nil
	}
	if response.StatusCode != http.StatusOK {
		s.recordStatus(status, false)
		return ErrCredentialUnavailable
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxAuthBytes+1))
	if err != nil || len(contents) > maxAuthBytes {
		s.recordStatus(StatusUnavailable, false)
		return ErrCredentialUnavailable
	}
	if _, err := writeCredential(s.authFile, contents, false); err != nil {
		s.recordStatus(StatusUnavailable, false)
		return ErrCredentialUnavailable
	}
	s.mu.Lock()
	s.etag = response.Header.Get("ETag")
	s.lastSuccess = time.Now().UTC()
	s.lastStatus = StatusReady
	s.mu.Unlock()
	return nil
}

func (s *SyncClient) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("sync interval must be positive")
	}
	if err := s.Sync(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Sync(ctx); errors.Is(err, ErrReauthenticationRequired) {
				return err
			}
		}
	}
}

func (s *SyncClient) Handler(maxStale time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeSafeJSON(response, http.StatusOK, StatusReady)
	})
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, _ *http.Request) {
		s.mu.RLock()
		lastSuccess, status := s.lastSuccess, s.lastStatus
		s.mu.RUnlock()
		if status == StatusReauthenticationRequired {
			writeSafeJSON(response, http.StatusServiceUnavailable, status)
			return
		}
		if lastSuccess.IsZero() || time.Since(lastSuccess) > maxStale {
			writeSafeJSON(response, http.StatusServiceUnavailable, StatusUnavailable)
			return
		}
		writeSafeJSON(response, http.StatusOK, StatusReady)
	})
	return mux
}

func (s *SyncClient) recordStatus(status string, success bool) {
	s.mu.Lock()
	changed := s.lastStatus != status
	s.lastStatus = status
	if success {
		s.lastSuccess = time.Now().UTC()
	}
	s.mu.Unlock()
	if changed {
		s.logger.Info("Codex credential sync status changed", "status", status)
	}
}
