package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	StatusReady                    = "ready"
	StatusNotBootstrapped          = "credential_not_bootstrapped"
	StatusReauthenticationRequired = "credential_reauthentication_required"
	StatusRefreshFailed            = "credential_refresh_failed"
	StatusUnavailable              = "credential_unavailable"
)

type coordinatorState struct {
	Status string
	Detail string
}

type Coordinator struct {
	authFile        string
	codexBinary     string
	refreshInterval time.Duration
	logger          *slog.Logger

	mu    sync.RWMutex
	state coordinatorState
}

func NewCoordinator(authFile, codexBinary string, refreshInterval time.Duration, logger *slog.Logger) (*Coordinator, error) {
	if authFile == "" || !filepath.IsAbs(authFile) || codexBinary == "" || !filepath.IsAbs(codexBinary) || refreshInterval <= 0 {
		return nil, fmt.Errorf("absolute credential and Codex paths and a positive refresh interval are required")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Coordinator{
		authFile: authFile, codexBinary: codexBinary, refreshInterval: refreshInterval, logger: logger,
		state: coordinatorState{Status: StatusNotBootstrapped},
	}, nil
}

func (c *Coordinator) Run(ctx context.Context) {
	c.maintain(ctx)
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.maintain(ctx)
		}
	}
}

func (c *Coordinator) maintain(ctx context.Context) {
	_, before, err := readMaster(c.authFile)
	if err != nil {
		if errors.Is(err, ErrAuthMissing) {
			c.setState(StatusNotBootstrapped, "")
		} else {
			c.setState(StatusUnavailable, "")
		}
		return
	}
	checkContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(checkContext, c.codexBinary,
		"-c", `cli_auth_credentials_store="file"`,
		"-c", `forced_login_method="chatgpt"`,
		"mcp", "list", "--json",
	)
	command.Env = append(os.Environ(), "CODEX_HOME="+filepath.Dir(c.authFile))
	command.Stdout = io.Discard
	var diagnostic cappedBuffer
	command.Stderr = &diagnostic
	commandError := command.Run()
	_, after, readError := readMaster(c.authFile)
	if readError != nil {
		c.setState(StatusUnavailable, "")
		return
	}
	diagnosticText := strings.ToLower(diagnostic.String())
	if strings.Contains(diagnosticText, "refresh token has expired") ||
		strings.Contains(diagnosticText, "refresh token was already used") ||
		strings.Contains(diagnosticText, "refresh token was revoked") {
		c.setState(StatusReauthenticationRequired, "")
		return
	}
	if after.ExpiresAt.After(time.Now().Add(2 * time.Minute)) {
		c.setState(StatusReady, "")
		return
	}
	if commandError != nil || !after.LastRefresh.After(before.LastRefresh) {
		c.setState(StatusRefreshFailed, "")
		return
	}
	c.setState(StatusReady, "")
}

func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeSafeJSON(response, http.StatusOK, StatusReady)
	})
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, _ *http.Request) {
		state := c.currentState()
		status := http.StatusOK
		if state.Status != StatusReady {
			status = http.StatusServiceUnavailable
		}
		writeSafeJSON(response, status, state.Status)
	})
	mux.HandleFunc("GET /v1/auth", c.handleAuth)
	return mux
}

func (c *Coordinator) handleAuth(response http.ResponseWriter, request *http.Request) {
	state := c.currentState()
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-Sandherd-Credential-Status", state.Status)
	if state.Status != StatusReady {
		writeSafeJSON(response, http.StatusServiceUnavailable, state.Status)
		return
	}
	contents, metadata, err := readMaster(c.authFile)
	if err != nil {
		c.setState(StatusUnavailable, "")
		response.Header().Set("X-Sandherd-Credential-Status", StatusUnavailable)
		writeSafeJSON(response, http.StatusServiceUnavailable, StatusUnavailable)
		return
	}
	if request.Header.Get("If-None-Match") == metadata.ETag {
		response.WriteHeader(http.StatusNotModified)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("ETag", metadata.ETag)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(contents)
}

func (c *Coordinator) setState(status, detail string) {
	c.mu.Lock()
	changed := c.state.Status != status
	c.state = coordinatorState{Status: status, Detail: detail}
	c.mu.Unlock()
	if changed {
		c.logger.Info("Codex credential status changed", "status", status)
	}
}

func (c *Coordinator) currentState() coordinatorState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

type cappedBuffer struct {
	contents []byte
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	const limit = 16 * 1024
	original := len(value)
	remaining := limit - len(b.contents)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		b.contents = append(b.contents, value...)
	}
	return original, nil
}

func (b *cappedBuffer) String() string { return string(b.contents) }

func writeSafeJSON(response http.ResponseWriter, status int, reason string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"status": reason})
}
