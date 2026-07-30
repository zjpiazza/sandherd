package codexauth

import (
	"context"
	"io"
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
