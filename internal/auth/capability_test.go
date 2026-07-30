package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func TestCapabilityScopeSignatureAndExpiry(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewSigner(privateKey, time.Minute)
	verifier, _ := NewVerifier(publicKey)
	now := time.Unix(1_800_000_000, 0)
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	token, err := signer.Mint("agent-a", "control", "request-a")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(token, "agent-a")
	if err != nil || claims.Role != "control" || claims.Scope != "terminal" || claims.RequestID != "request-a" {
		t.Fatalf("claims = %#v, error %v", claims, err)
	}
	if _, err := verifier.Verify(token, "agent-b"); err == nil {
		t.Fatal("capability was accepted for another agent")
	}
	parts := strings.Split(token, ".")
	replacement := "A"
	if strings.HasSuffix(parts[1], replacement) {
		replacement = "B"
	}
	parts[1] = parts[1][:len(parts[1])-1] + replacement
	if _, err := verifier.Verify(strings.Join(parts, "."), "agent-a"); err == nil {
		t.Fatal("tampered capability was accepted")
	}
	verifier.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := verifier.Verify(token, "agent-a"); err == nil {
		t.Fatal("expired capability was accepted")
	}
}
