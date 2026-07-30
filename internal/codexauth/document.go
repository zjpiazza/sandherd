// Package codexauth implements single-writer Codex ChatGPT credential coordination.
package codexauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxAuthBytes = 128 * 1024

var (
	ErrAuthMissing        = errors.New("credential is not bootstrapped")
	ErrAuthInvalid        = errors.New("credential is invalid")
	ErrRefreshTokenAbsent = errors.New("credential has no refresh token")
)

type documentMetadata struct {
	LastRefresh time.Time
	ExpiresAt   time.Time
	ETag        string
}

func readMaster(path string) ([]byte, documentMetadata, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, documentMetadata{}, ErrAuthMissing
	}
	if err != nil {
		return nil, documentMetadata{}, fmt.Errorf("read credential: %w", err)
	}
	_, snapshot, metadata, err := validatedDocuments(contents, true)
	return snapshot, metadata, err
}

func validateSnapshot(contents []byte) (documentMetadata, error) {
	_, metadata, err := sanitizeDocument(contents, false)
	return metadata, err
}

// sanitizeDocument validates ChatGPT file auth and returns the sandbox form.
// The sandbox form never contains the platform refresh token.
func sanitizeDocument(contents []byte, requireRefreshToken bool) ([]byte, documentMetadata, error) {
	_, snapshot, metadata, err := validatedDocuments(contents, requireRefreshToken)
	return snapshot, metadata, err
}

func validatedDocuments(contents []byte, requireRefreshToken bool) ([]byte, []byte, documentMetadata, error) {
	if len(contents) == 0 || len(contents) > maxAuthBytes {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	mode, _ := document["auth_mode"].(string)
	if mode != "" && mode != "chatgpt" {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	if apiKey, _ := document["OPENAI_API_KEY"].(string); apiKey != "" {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	tokens, ok := document["tokens"].(map[string]any)
	if !ok {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	accessToken, _ := tokens["access_token"].(string)
	if len(accessToken) < 16 || len(accessToken) > 64*1024 {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	idToken, _ := tokens["id_token"].(string)
	if len(idToken) < 16 || len(idToken) > 64*1024 {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	refreshToken, _ := tokens["refresh_token"].(string)
	if requireRefreshToken && refreshToken == "" {
		return nil, nil, documentMetadata{}, ErrRefreshTokenAbsent
	}
	if len(refreshToken) > 64*1024 {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	accountID, _ := tokens["account_id"].(string)
	if len(accountID) > 4*1024 {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	lastRefreshValue, _ := document["last_refresh"].(string)
	lastRefresh, err := time.Parse(time.RFC3339Nano, lastRefreshValue)
	if err != nil {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	expiresAt, err := jwtExpiration(accessToken)
	if err != nil {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	masterTokens := map[string]any{
		"id_token": idToken, "access_token": accessToken, "refresh_token": refreshToken,
	}
	if accountID != "" {
		masterTokens["account_id"] = accountID
	}
	masterDocument := map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": masterTokens, "last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
	}
	master, err := json.MarshalIndent(masterDocument, "", "  ")
	if err != nil {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	master = append(master, '\n')
	masterTokens["refresh_token"] = ""
	snapshot, err := json.MarshalIndent(masterDocument, "", "  ")
	if err != nil {
		return nil, nil, documentMetadata{}, ErrAuthInvalid
	}
	snapshot = append(snapshot, '\n')
	digest := sha256.Sum256(snapshot)
	metadata := documentMetadata{
		LastRefresh: lastRefresh.UTC(), ExpiresAt: expiresAt.UTC(), ETag: `"` + hex.EncodeToString(digest[:]) + `"`,
	}
	return master, snapshot, metadata, nil
}

func jwtExpiration(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, ErrAuthInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 64*1024 {
		return time.Time{}, ErrAuthInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var claims map[string]any
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}, ErrAuthInvalid
	}
	expiration, ok := claims["exp"].(json.Number)
	if !ok {
		return time.Time{}, ErrAuthInvalid
	}
	seconds, err := strconv.ParseInt(expiration.String(), 10, 64)
	if err != nil || seconds < 1 {
		return time.Time{}, ErrAuthInvalid
	}
	return time.Unix(seconds, 0), nil
}

func writeCredential(path string, contents []byte, requireRefreshToken bool) (documentMetadata, error) {
	var normalized []byte
	var metadata documentMetadata
	var err error
	if requireRefreshToken {
		normalized, _, metadata, err = validatedDocuments(contents, true)
	} else {
		_, normalized, metadata, err = validatedDocuments(contents, false)
	}
	if err != nil {
		return documentMetadata{}, err
	}
	parent := filepath.Dir(filepath.Clean(path))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return documentMetadata{}, fmt.Errorf("prepare credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".auth-*")
	if err != nil {
		return documentMetadata{}, fmt.Errorf("create credential update: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return documentMetadata{}, fmt.Errorf("protect credential update: %w", err)
	}
	if _, err := temporary.Write(normalized); err != nil {
		temporary.Close()
		return documentMetadata{}, fmt.Errorf("write credential update: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return documentMetadata{}, fmt.Errorf("sync credential update: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return documentMetadata{}, fmt.Errorf("close credential update: %w", err)
	}
	if err := os.Rename(temporaryPath, filepath.Clean(path)); err != nil {
		return documentMetadata{}, fmt.Errorf("publish credential update: %w", err)
	}
	return metadata, nil
}
