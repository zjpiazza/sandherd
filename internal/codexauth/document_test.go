package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRefreshToken = "refresh-token-that-must-never-leave-the-coordinator"

func testAuthDocument(t *testing.T, expiresAt time.Time, refreshToken string) []byte {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{"exp": expiresAt.Unix(), "sub": "test-user"})
	if err != nil {
		t.Fatal(err)
	}
	accessToken := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	document, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"id_token":      accessToken,
			"account_id":    "account-test",
		},
		"last_refresh": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestSanitizeDocumentRemovesOnlyRefreshAuthority(t *testing.T) {
	document := testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken)
	sanitized, metadata, err := sanitizeDocument(document, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sanitized), testRefreshToken) || !strings.Contains(string(sanitized), `"refresh_token": ""`) {
		t.Fatalf("sandbox credential was not sanitized: %s", sanitized)
	}
	if metadata.ExpiresAt.Before(time.Now()) || metadata.ETag == "" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if _, _, err := sanitizeDocument(testAuthDocument(t, time.Now().Add(time.Hour), ""), true); err != ErrRefreshTokenAbsent {
		t.Fatalf("missing refresh error = %v", err)
	}
}

func TestSanitizeDocumentSharesOnlyCodexAuthenticationFields(t *testing.T) {
	document := map[string]any{}
	if err := json.Unmarshal(testAuthDocument(t, time.Now().Add(time.Hour), testRefreshToken), &document); err != nil {
		t.Fatal(err)
	}
	document["unexpected_secret"] = "must-not-leave-coordinator"
	tokens := document["tokens"].(map[string]any)
	tokens["unexpected_token"] = "must-not-leave-coordinator"
	contents, _ := json.Marshal(document)
	snapshot, _, err := sanitizeDocument(contents, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot), "must-not-leave-coordinator") {
		t.Fatalf("unexpected credential field reached sandbox: %s", snapshot)
	}
}

func TestWriteCredentialIsAtomicAndProtected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "auth.json")
	if _, err := writeCredential(path, testAuthDocument(t, time.Now().Add(time.Hour), ""), false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o", info.Mode().Perm())
	}
}

func TestDocumentRejectsWrongAuthModesAndMalformedTokens(t *testing.T) {
	tests := [][]byte{
		[]byte(`{"auth_mode":"api","OPENAI_API_KEY":"secret"}`),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"not-a-jwt","refresh_token":"refresh"},"last_refresh":"2026-01-01T00:00:00Z"}`),
		[]byte(`not-json`),
	}
	for _, document := range tests {
		if _, _, err := sanitizeDocument(document, true); err == nil {
			t.Fatalf("invalid document accepted: %s", document)
		}
	}
}
