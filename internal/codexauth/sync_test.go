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
	"sync"
	"testing"
	"time"
)

func TestSyncClientSeedsAndUpdatesOnlyAuthFile(t *testing.T) {
	var current = testAuthDocument(t, time.Now().Add(time.Hour), "")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Sandherd-Credential-Status", StatusReady)
		response.Header().Set("ETag", `"version"`)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(current)
	}))
	defer server.Close()
	home := t.TempDir()
	authFile := filepath.Join(home, ".codex", "auth.json")
	sessionFile := filepath.Join(home, ".codex", "sessions", "retained.jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte("session-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewSyncClient(server.URL, authFile, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(authFile)
	if strings.Contains(string(contents), testRefreshToken) {
		t.Fatal("refresh token reached sandbox auth file")
	}
	session, _ := os.ReadFile(sessionFile)
	if string(session) != "session-canary" {
		t.Fatalf("session state changed: %q", session)
	}
	probe := httptest.NewRecorder()
	client.Handler(time.Minute).ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if probe.Code != http.StatusOK {
		t.Fatalf("readiness = %d %s", probe.Code, probe.Body.String())
	}
}

func TestSyncClientRejectsExpiredAndUntrustedStatusWithoutLeaking(t *testing.T) {
	for _, scenario := range []string{"expired", "untrusted-status", "refresh-failed"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				switch scenario {
				case "expired":
					response.Header().Set("X-Sandherd-Credential-Status", StatusReady)
					_, _ = response.Write(testAuthDocument(t, time.Now().Add(-time.Minute), ""))
				case "untrusted-status":
					response.Header().Set("X-Sandherd-Credential-Status", "sensitive-token-value /private/auth.json")
					response.WriteHeader(http.StatusServiceUnavailable)
				case "refresh-failed":
					response.Header().Set("X-Sandherd-Credential-Status", StatusRefreshFailed)
					response.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			var logs bytes.Buffer
			client, err := NewSyncClient(server.URL, filepath.Join(t.TempDir(), "auth.json"), time.Second, slog.New(slog.NewJSONHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			err = client.Sync(context.Background())
			if err == nil || (scenario == "refresh-failed" && err != ErrRefreshFailed) {
				t.Fatalf("unexpected sync result: %v", err)
			}
			probe := httptest.NewRecorder()
			client.Handler(time.Minute).ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if probe.Code != http.StatusServiceUnavailable || strings.Contains(logs.String()+probe.Body.String(), "sensitive-token-value") || strings.Contains(logs.String()+probe.Body.String(), "/private/") {
				t.Fatal("invalid credential was ready or upstream diagnostics leaked")
			}
			if _, err := os.Stat(client.authFile); !os.IsNotExist(err) {
				t.Fatal("invalid credential was written")
			}
		})
	}
}

func TestSyncClientRepairsMissingSnapshotAfterNotModified(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "master.json")
	if _, err := writeCredential(authFile, testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken), true); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := NewCoordinator(authFile, "/bin/true", time.Minute, nil)
	coordinator.setState(StatusReady, "")
	server := httptest.NewServer(coordinator.Handler())
	defer server.Close()
	client, _ := NewSyncClient(server.URL+"/v1/auth", filepath.Join(t.TempDir(), "auth.json"), time.Second, nil)
	if err := client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(client.authFile); err != nil {
		t.Fatal(err)
	}
	if err := client.Sync(context.Background()); err != ErrCredentialUnavailable {
		t.Fatalf("missing snapshot accepted: %v", err)
	}
	if err := client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(client.authFile); err != nil {
		t.Fatal("snapshot was not restored")
	}
}

func TestSyncClientOutageExitsAfterMaximumStaleness(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	document := testAuthDocument(t, time.Now().Add(time.Hour), "")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests == 1 {
			_, _ = response.Write(document)
			return
		}
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, _ := NewSyncClient(server.URL, filepath.Join(t.TempDir(), "auth.json"), time.Second, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Run(ctx, 5*time.Millisecond, 25*time.Millisecond); err != ErrCredentialUnavailable {
		t.Fatalf("stale credential did not surface a stable failure: %v", err)
	}
}

func TestCredentialRecoversAfterReauthentication(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "master.json")
	if _, err := writeCredential(authFile, testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken), true); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := NewCoordinator(authFile, "/bin/true", time.Minute, nil)
	coordinator.setState(StatusReady, "")
	server := httptest.NewServer(coordinator.Handler())
	defer server.Close()
	client, _ := NewSyncClient(server.URL+"/v1/auth", filepath.Join(t.TempDir(), "auth.json"), time.Second, nil)
	if err := client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	coordinator.setState(StatusReauthenticationRequired, "")
	if err := client.Sync(context.Background()); err != ErrReauthenticationRequired {
		t.Fatalf("revocation not propagated: %v", err)
	}
	if _, err := writeCredential(authFile, testAuthDocument(t, time.Now().Add(2*time.Hour), "new-platform-refresh-token"), true); err != nil {
		t.Fatal(err)
	}
	coordinator.setState(StatusReady, "")
	if err := client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe := httptest.NewRecorder()
	client.Handler(time.Minute).ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if probe.Code != http.StatusOK {
		t.Fatal("repaired credential did not restore readiness")
	}
}

func TestSyncClientClassifiesReauthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("X-Sandherd-Credential-Status", StatusReauthenticationRequired)
		writeSafeJSON(response, http.StatusServiceUnavailable, StatusReauthenticationRequired)
	}))
	defer server.Close()
	client, err := NewSyncClient(server.URL, filepath.Join(t.TempDir(), "auth.json"), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Sync(context.Background()); err != ErrReauthenticationRequired {
		t.Fatalf("sync error = %v", err)
	}
	probe := httptest.NewRecorder()
	client.Handler(time.Minute).ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	body, _ := io.ReadAll(probe.Body)
	if probe.Code != http.StatusServiceUnavailable || !strings.Contains(string(body), StatusReauthenticationRequired) {
		t.Fatalf("readiness = %d %s", probe.Code, body)
	}
}

func TestCoordinatorRefreshSnapshotConvergesConcurrentAndNewSandboxes(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	first := testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken)
	if _, err := writeCredential(authFile, first, true); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(authFile, "/bin/true", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.setState(StatusReady, "")
	server := httptest.NewServer(coordinator.Handler())
	defer server.Close()

	clients := make([]*SyncClient, 2)
	for index := range clients {
		clients[index], err = NewSyncClient(server.URL+"/v1/auth", filepath.Join(t.TempDir(), ".codex", "auth.json"), time.Second, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	syncAll := func(list []*SyncClient) {
		t.Helper()
		var group sync.WaitGroup
		errors := make(chan error, len(list))
		for _, client := range list {
			group.Add(1)
			go func(client *SyncClient) {
				defer group.Done()
				errors <- client.Sync(context.Background())
			}(client)
		}
		group.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	syncAll(clients)

	second := testAuthDocument(t, time.Now().Add(2*time.Hour), "rotated-refresh-token-must-stay-central")
	if _, err := writeCredential(authFile, second, true); err != nil {
		t.Fatal(err)
	}
	third, err := NewSyncClient(server.URL+"/v1/auth", filepath.Join(t.TempDir(), ".codex", "auth.json"), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	syncAll(append(clients, third))

	_, expected, _, err := validatedDocuments(second, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range append(clients, third) {
		contents, err := os.ReadFile(client.authFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != string(expected) || strings.Contains(string(contents), "rotated-refresh-token") {
			t.Fatalf("sandbox snapshot did not converge safely: %s", contents)
		}
	}
	master, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(master), "rotated-refresh-token-must-stay-central") {
		t.Fatal("coordinator lost refresh authority")
	}
}
