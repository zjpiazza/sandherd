package codexauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrCredentialUnavailable    = errors.New("credential is unavailable")
	ErrReauthenticationRequired = errors.New("credential reauthentication is required")
	ErrRefreshFailed            = errors.New("credential refresh failed")
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
	expiresAt   time.Time
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
	status := safeCredentialStatus(response.Header.Get("X-Sandherd-Credential-Status"))
	if status == StatusReauthenticationRequired {
		s.recordStatus(status, false)
		return ErrReauthenticationRequired
	}
	if status == StatusRefreshFailed {
		s.recordStatus(status, false)
		return ErrRefreshFailed
	}
	if response.StatusCode == http.StatusNotModified {
		contents, readErr := os.ReadFile(s.authFile)
		metadata, validationErr := validateSnapshot(contents)
		if readErr != nil || validationErr != nil || metadata.ETag != etag || !metadata.ExpiresAt.After(time.Now()) {
			s.mu.Lock()
			s.etag = ""
			s.mu.Unlock()
			s.recordStatus(StatusUnavailable, false)
			return ErrCredentialUnavailable
		}
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
	metadata, err := validateSnapshot(contents)
	if err != nil || !metadata.ExpiresAt.After(time.Now()) {
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
	s.expiresAt = metadata.ExpiresAt
	s.mu.Unlock()
	return nil
}

func (s *SyncClient) Run(ctx context.Context, interval, maxStale time.Duration) error {
	if interval <= 0 || maxStale <= 0 {
		return fmt.Errorf("sync interval and maximum age must be positive")
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
			if err := s.Sync(ctx); err != nil {
				if errors.Is(err, ErrReauthenticationRequired) || errors.Is(err, ErrRefreshFailed) || !s.usable(maxStale) {
					return err
				}
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
		status := s.lastStatus
		s.mu.RUnlock()
		if status == StatusReauthenticationRequired || status == StatusRefreshFailed {
			writeSafeJSON(response, http.StatusServiceUnavailable, status)
			return
		}
		if !s.usable(maxStale) {
			writeSafeJSON(response, http.StatusServiceUnavailable, StatusUnavailable)
			return
		}
		writeSafeJSON(response, http.StatusOK, StatusReady)
	})
	return mux
}

func (s *SyncClient) usable(maxStale time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.lastSuccess.IsZero() && time.Since(s.lastSuccess) <= maxStale && s.expiresAt.After(time.Now())
}

// Remote headers are not log-safe. Only these fixed reasons may reach logs
// and probes, even if a proxy returns an arbitrary diagnostic in the header.
func safeCredentialStatus(status string) string {
	switch status {
	case StatusReady, StatusNotBootstrapped, StatusReauthenticationRequired, StatusRefreshFailed:
		return status
	default:
		return StatusUnavailable
	}
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
