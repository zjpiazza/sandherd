package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	aliceToken = "alice-control-token-value-that-is-long-enough"
	bobToken   = "bob-observe-token-value-that-is-long-enough"
)

func TestFileAuthenticatorPrincipalsPermissionsProfilesAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "principals.json")
	writePrincipalFile(t, path, `{"version":1,"principals":[{"id":"alice@example.com","token":"`+aliceToken+`","permissions":["control"],"secretProfiles":["personal"],"credentialProfiles":["subscription"]},{"id":"bob","token":"`+bobToken+`","permissions":["observe"]}]}`)
	authenticator, err := NewFileAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := authenticator.Authenticate(context.Background(), aliceToken)
	if err != nil || alice.ID != "alice@example.com" || !alice.CanControl() || !alice.CanObserve() || !alice.AllowsSecretProfile("personal") || alice.AllowsSecretProfile("other") || !alice.AllowsCredentialProfile("subscription") || alice.AllowsCredentialProfile("other") {
		t.Fatalf("alice = %#v, %v", alice, err)
	}
	bob, err := authenticator.Authenticate(context.Background(), bobToken)
	if err != nil || bob.ID != "bob" || bob.CanControl() || !bob.CanObserve() || bob.AllowsSecretProfile("personal") {
		t.Fatalf("bob = %#v, %v", bob, err)
	}
	if _, err := authenticator.Authenticate(context.Background(), "not-a-valid-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("invalid credential error = %v", err)
	}

	rotated := "alice-rotated-token-value-that-is-long-enough"
	writePrincipalFile(t, path, `{"version":1,"principals":[{"id":"alice@example.com","token":"`+rotated+`","permissions":["control"]}]}`)
	if _, err := authenticator.Authenticate(context.Background(), aliceToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old credential after rotation = %v", err)
	}
	if principal, err := authenticator.Authenticate(context.Background(), rotated); err != nil || principal.ID != "alice@example.com" {
		t.Fatalf("rotated credential = %#v, %v", principal, err)
	}
}

func TestFileAuthenticatorRejectsUnsafeConfigurationWithoutLeakingTokens(t *testing.T) {
	tests := []string{
		`{"version":2,"principals":[{"id":"alice","token":"` + aliceToken + `","permissions":["control"]}]}`,
		`{"version":1,"principals":[{"id":"bad id","token":"` + aliceToken + `","permissions":["control"]}]}`,
		`{"version":1,"principals":[{"id":"alice","token":"short","permissions":["control"]}]}`,
		`{"version":1,"principals":[{"id":"alice","token":"` + aliceToken + `","permissions":["admin"]}]}`,
		`{"version":1,"principals":[{"id":"alice","token":"` + aliceToken + `","permissions":["control"],"secretProfiles":["Bad"]}]}`,
		`{"version":1,"principals":[{"id":"alice","token":"` + aliceToken + `","permissions":["control"],"credentialProfiles":["Bad"]}]}`,
		`{"version":1,"principals":[{"id":"alice","token":"` + aliceToken + `","permissions":["control"]},{"id":"bob","token":"` + aliceToken + `","permissions":["observe"]}]}`,
	}
	for _, contents := range tests {
		path := filepath.Join(t.TempDir(), "principals.json")
		writePrincipalFile(t, path, contents)
		_, err := NewFileAuthenticator(path)
		if err == nil {
			t.Fatalf("unsafe configuration accepted: %s", contents)
		}
		if strings.Contains(err.Error(), aliceToken) {
			t.Fatalf("configuration error leaked token: %v", err)
		}
	}
}

func writePrincipalFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
