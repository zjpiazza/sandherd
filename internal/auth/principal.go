package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxPrincipalFileBytes = 1024 * 1024

var (
	ErrUnauthenticated = errors.New("credential is not authenticated")
	ErrUnavailable     = errors.New("authentication service is unavailable")
	principalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	profileNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

type Permission string

const (
	PermissionObserve Permission = "observe"
	PermissionControl Permission = "control"
)

type Principal struct {
	ID                 string
	permissions        map[Permission]struct{}
	secretProfiles     map[string]struct{}
	credentialProfiles map[string]struct{}
}

func NewPrincipal(id string, permissions []Permission, secretProfiles []string) (Principal, error) {
	return NewPrincipalWithCredentialProfiles(id, permissions, secretProfiles, nil)
}

func NewPrincipalWithCredentialProfiles(id string, permissions []Permission, secretProfiles, credentialProfiles []string) (Principal, error) {
	if !principalIDPattern.MatchString(id) {
		return Principal{}, fmt.Errorf("principal ID is invalid")
	}
	if len(permissions) == 0 {
		return Principal{}, fmt.Errorf("at least one principal permission is required")
	}
	result := Principal{
		ID: id, permissions: make(map[Permission]struct{}, len(permissions)),
		secretProfiles:     make(map[string]struct{}, len(secretProfiles)),
		credentialProfiles: make(map[string]struct{}, len(credentialProfiles)),
	}
	for _, permission := range permissions {
		if permission != PermissionObserve && permission != PermissionControl {
			return Principal{}, fmt.Errorf("principal permission is invalid")
		}
		if _, exists := result.permissions[permission]; exists {
			return Principal{}, fmt.Errorf("principal permission is repeated")
		}
		result.permissions[permission] = struct{}{}
	}
	for _, profile := range secretProfiles {
		if !profileNamePattern.MatchString(profile) {
			return Principal{}, fmt.Errorf("principal secret profile is invalid")
		}
		if _, exists := result.secretProfiles[profile]; exists {
			return Principal{}, fmt.Errorf("principal secret profile is repeated")
		}
		result.secretProfiles[profile] = struct{}{}
	}
	for _, profile := range credentialProfiles {
		if !profileNamePattern.MatchString(profile) {
			return Principal{}, fmt.Errorf("principal credential profile is invalid")
		}
		if _, exists := result.credentialProfiles[profile]; exists {
			return Principal{}, fmt.Errorf("principal credential profile is repeated")
		}
		result.credentialProfiles[profile] = struct{}{}
	}
	return result, nil
}

func (p Principal) CanObserve() bool {
	return p.CanControl() || p.hasPermission(PermissionObserve)
}

func (p Principal) CanControl() bool {
	return p.hasPermission(PermissionControl)
}

func (p Principal) AllowsSecretProfile(profile string) bool {
	if profile == "" {
		return true
	}
	_, ok := p.secretProfiles[profile]
	return ok
}

func (p Principal) AllowsCredentialProfile(profile string) bool {
	if profile == "" {
		return true
	}
	_, ok := p.credentialProfiles[profile]
	return ok
}

func (p Principal) hasPermission(permission Permission) bool {
	_, ok := p.permissions[permission]
	return ok
}

type Authenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}

type FileAuthenticator struct {
	path string
}

type principalFile struct {
	Version    int                   `json:"version"`
	Principals []principalCredential `json:"principals"`
}

type principalCredential struct {
	ID                 string   `json:"id"`
	Token              string   `json:"token"`
	Permissions        []string `json:"permissions"`
	SecretProfiles     []string `json:"secretProfiles,omitempty"`
	CredentialProfiles []string `json:"credentialProfiles,omitempty"`
}

func NewFileAuthenticator(path string) (*FileAuthenticator, error) {
	if path == "" {
		return nil, fmt.Errorf("principal credential file is required")
	}
	authenticator := &FileAuthenticator{path: filepath.Clean(path)}
	if _, err := authenticator.load(); err != nil {
		return nil, err
	}
	return authenticator, nil
}

func (a *FileAuthenticator) Authenticate(_ context.Context, bearer string) (Principal, error) {
	if len(bearer) < 32 || len(bearer) > 4096 {
		return Principal{}, ErrUnauthenticated
	}
	credentials, err := a.load()
	if err != nil {
		return Principal{}, fmt.Errorf("%w: principal credentials could not be loaded", ErrUnavailable)
	}
	matched := -1
	for index := range credentials.Principals {
		candidate := credentials.Principals[index].Token
		sameLength := subtle.ConstantTimeEq(int32(len(candidate)), int32(len(bearer)))
		comparison := 0
		if len(candidate) == len(bearer) {
			comparison = subtle.ConstantTimeCompare([]byte(candidate), []byte(bearer))
		}
		if sameLength&comparison == 1 {
			matched = index
		}
	}
	if matched < 0 {
		return Principal{}, ErrUnauthenticated
	}
	return credentials.Principals[matched].principal(), nil
}

func (a *FileAuthenticator) load() (principalFile, error) {
	file, err := os.Open(a.path)
	if err != nil {
		return principalFile{}, fmt.Errorf("open principal credential file: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxPrincipalFileBytes+1))
	if err != nil {
		return principalFile{}, fmt.Errorf("read principal credential file: %w", err)
	}
	if len(contents) > maxPrincipalFileBytes {
		return principalFile{}, fmt.Errorf("principal credential file exceeds %d bytes", maxPrincipalFileBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var result principalFile
	if err := decoder.Decode(&result); err != nil {
		return principalFile{}, fmt.Errorf("decode principal credential file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return principalFile{}, fmt.Errorf("principal credential file must contain one JSON value")
	}
	if err := result.validate(); err != nil {
		return principalFile{}, err
	}
	return result, nil
}

func (f principalFile) validate() error {
	if f.Version != 1 {
		return fmt.Errorf("principal credential file version must be 1")
	}
	if len(f.Principals) == 0 || len(f.Principals) > 1024 {
		return fmt.Errorf("principal credential file must contain between 1 and 1024 principals")
	}
	identifiers := make(map[string]struct{}, len(f.Principals))
	tokens := make(map[string]struct{}, len(f.Principals))
	for index := range f.Principals {
		credential := &f.Principals[index]
		if !principalIDPattern.MatchString(credential.ID) {
			return fmt.Errorf("principal %d has an invalid ID", index)
		}
		if _, exists := identifiers[credential.ID]; exists {
			return fmt.Errorf("principal IDs must be unique")
		}
		identifiers[credential.ID] = struct{}{}
		if len(credential.Token) < 32 || len(credential.Token) > 4096 || strings.TrimSpace(credential.Token) != credential.Token || strings.IndexFunc(credential.Token, func(character rune) bool { return character <= 0x20 || character == 0x7f }) >= 0 {
			return fmt.Errorf("principal %d has an invalid bearer token", index)
		}
		if _, exists := tokens[credential.Token]; exists {
			return fmt.Errorf("principal bearer tokens must be unique")
		}
		tokens[credential.Token] = struct{}{}
		if len(credential.Permissions) == 0 {
			return fmt.Errorf("principal %d must have at least one permission", index)
		}
		permissionSet := make(map[string]struct{}, len(credential.Permissions))
		for _, permission := range credential.Permissions {
			if permission != string(PermissionObserve) && permission != string(PermissionControl) {
				return fmt.Errorf("principal %d has an invalid permission", index)
			}
			if _, exists := permissionSet[permission]; exists {
				return fmt.Errorf("principal %d repeats a permission", index)
			}
			permissionSet[permission] = struct{}{}
		}
		profileSet := make(map[string]struct{}, len(credential.SecretProfiles))
		for _, profile := range credential.SecretProfiles {
			if !profileNamePattern.MatchString(profile) {
				return fmt.Errorf("principal %d has an invalid secret profile", index)
			}
			if _, exists := profileSet[profile]; exists {
				return fmt.Errorf("principal %d repeats a secret profile", index)
			}
			profileSet[profile] = struct{}{}
		}
		credentialProfileSet := make(map[string]struct{}, len(credential.CredentialProfiles))
		for _, profile := range credential.CredentialProfiles {
			if !profileNamePattern.MatchString(profile) {
				return fmt.Errorf("principal %d has an invalid credential profile", index)
			}
			if _, exists := credentialProfileSet[profile]; exists {
				return fmt.Errorf("principal %d repeats a credential profile", index)
			}
			credentialProfileSet[profile] = struct{}{}
		}
	}
	return nil
}

func (c principalCredential) principal() Principal {
	permissions := make([]Permission, 0, len(c.Permissions))
	for _, permission := range c.Permissions {
		permissions = append(permissions, Permission(permission))
	}
	result, _ := NewPrincipalWithCredentialProfiles(c.ID, permissions, c.SecretProfiles, c.CredentialProfiles)
	return result
}
