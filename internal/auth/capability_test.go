package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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
	if _, err := verifier.Verify(token, "agent-a"); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("replayed capability error = %v", err)
	}

	wrongAgentToken, _ := signer.Mint("agent-a", "control", "request-wrong-agent")
	if _, err := verifier.Verify(wrongAgentToken, "agent-b"); err == nil {
		t.Fatal("capability was accepted for another agent")
	}
	wrongScopeToken, _ := signer.Mint("agent-a", "control", "request-wrong-scope")
	if _, err := verifier.VerifyFor(wrongScopeToken, "agent-a", "signal"); err == nil {
		t.Fatal("terminal capability was accepted for another operation")
	}
	wrongAudienceToken := signedCapability(t, privateKey, Claims{
		Issuer: capabilityIssuer, Audience: "another-runner", Subject: "agent-a", Role: "control", Scope: "terminal",
		RequestID: "request-wrong-audience", TokenID: "wrong-audience", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	})
	if _, err := verifier.Verify(wrongAudienceToken, "agent-a"); err == nil {
		t.Fatal("capability with another audience was accepted")
	}

	tamperToken, _ := signer.Mint("agent-a", "control", "request-tamper")
	parts := strings.Split(tamperToken, ".")
	replacement := "A"
	if strings.HasSuffix(parts[1], replacement) {
		replacement = "B"
	}
	parts[1] = parts[1][:len(parts[1])-1] + replacement
	if _, err := verifier.Verify(strings.Join(parts, "."), "agent-a"); err == nil {
		t.Fatal("tampered capability was accepted")
	}
	expiredToken, _ := signer.Mint("agent-a", "control", "request-expiry")
	verifier.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := verifier.Verify(expiredToken, "agent-a"); err == nil {
		t.Fatal("expired capability was accepted")
	}
}

func signedCapability(t *testing.T, privateKey ed25519.PrivateKey, claims Claims) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "EdDSA", "typ": "SHCAP", "v": 1})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := rawURLEncoding.EncodeToString(header) + "." + rawURLEncoding.EncodeToString(payload)
	return unsigned + "." + rawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(unsigned)))
}
