package codexauth

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorDistributesRedactedCredentialAndSafeReadiness(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(authFile, "/bin/true", time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	coordinator.setState(StatusReady, "")
	server := httptest.NewServer(coordinator.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/auth")
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(string(contents), testRefreshToken) || response.Header.Get("ETag") == "" {
		t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, contents)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/auth", nil)
	request.Header.Set("If-None-Match", response.Header.Get("ETag"))
	unchanged, _ := http.DefaultClient.Do(request)
	unchanged.Body.Close()
	if unchanged.StatusCode != http.StatusNotModified {
		t.Fatalf("unchanged status = %d", unchanged.StatusCode)
	}

	coordinator.setState(StatusReauthenticationRequired, "sensitive detail")
	degraded, _ := http.Get(server.URL + "/readyz")
	degradedBody, _ := io.ReadAll(degraded.Body)
	degraded.Body.Close()
	if degraded.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(degradedBody), "sensitive") || !strings.Contains(string(degradedBody), StatusReauthenticationRequired) {
		t.Fatalf("degraded status=%d body=%s", degraded.StatusCode, degradedBody)
	}
}

func TestCoordinatorMaintenanceRecognizesRevocationWithoutLoggingToolOutput(t *testing.T) {
	directory := t.TempDir()
	authFile := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(authFile, testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeCodex := filepath.Join(directory, "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nprintf '%s\\n' 'Your access token could not be refreshed because your refresh token was revoked. sensitive-token-value' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	coordinator, err := NewCoordinator(authFile, fakeCodex, time.Minute, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	coordinator.maintain(context.Background())
	if coordinator.currentState().Status != StatusReauthenticationRequired {
		t.Fatalf("state = %#v", coordinator.currentState())
	}
	if strings.Contains(logs.String(), "sensitive-token-value") || strings.Contains(logs.String(), testRefreshToken) {
		t.Fatalf("credential material leaked into logs: %s", logs.String())
	}
}

func TestCoordinatorStartsUnreadyWithoutCredential(t *testing.T) {
	coordinator, err := NewCoordinator(filepath.Join(t.TempDir(), "auth.json"), "/bin/true", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.maintain(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1/auth", nil)
	response := httptest.NewRecorder()
	coordinator.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), coordinator.authFile) {
		t.Fatalf("response status=%d body=%s", response.Code, response.Body.String())
	}
}
