package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CapabilityHeader   = "X-Sandherd-Capability"
	capabilityIssuer   = "sandherd-control-plane"
	capabilityAudience = "sandherd-runner"
)

var rawURLEncoding = base64.RawURLEncoding

type Claims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	Role      string `json:"role"`
	Scope     string `json:"scope"`
	RequestID string `json:"requestId"`
	TokenID   string `json:"jti"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

type Signer struct {
	privateKey ed25519.PrivateKey
	ttl        time.Duration
	now        func() time.Time
}

func NewSigner(privateKey ed25519.PrivateKey, ttl time.Duration) (*Signer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("capability TTL must be positive")
	}
	return &Signer{privateKey: privateKey, ttl: ttl, now: time.Now}, nil
}

func (s *Signer) Mint(agentID, role, requestID string) (string, error) {
	if agentID == "" || requestID == "" || (role != "control" && role != "observe") {
		return "", fmt.Errorf("agent ID, request ID, and a valid role are required")
	}
	now := s.now().UTC()
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return "", fmt.Errorf("create capability identifier: %w", err)
	}
	claims := Claims{
		Issuer: capabilityIssuer, Audience: capabilityAudience, Subject: agentID,
		Role: role, Scope: "terminal", RequestID: requestID, TokenID: rawURLEncoding.EncodeToString(identifier),
		IssuedAt: now.Unix(), ExpiresAt: now.Add(s.ttl).Unix(),
	}
	header, _ := json.Marshal(map[string]any{"alg": "EdDSA", "typ": "SHCAP", "v": 1})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode capability: %w", err)
	}
	unsigned := rawURLEncoding.EncodeToString(header) + "." + rawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(s.privateKey, []byte(unsigned))
	return unsigned + "." + rawURLEncoding.EncodeToString(signature), nil
}

type Verifier struct {
	publicKey ed25519.PublicKey
	now       func() time.Time
}

func NewVerifier(publicKey ed25519.PublicKey) (*Verifier, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid Ed25519 public key")
	}
	return &Verifier{publicKey: publicKey, now: time.Now}, nil
}

func (v *Verifier) Verify(token, expectedAgentID string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("capability is malformed")
	}
	headerBytes, err := rawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("capability header is malformed")
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		Version   int    `json:"v"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "EdDSA" || header.Type != "SHCAP" || header.Version != 1 {
		return Claims{}, errors.New("capability header is invalid")
	}
	signature, err := rawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(v.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, errors.New("capability signature is invalid")
	}
	payload, err := rawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("capability payload is malformed")
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, errors.New("capability claims are malformed")
	}
	now := v.now().Unix()
	if claims.Issuer != capabilityIssuer || claims.Audience != capabilityAudience || claims.Subject != expectedAgentID || claims.RequestID == "" || claims.TokenID == "" {
		return Claims{}, errors.New("capability scope is invalid")
	}
	if (claims.Role != "control" && claims.Role != "observe") || claims.Scope != "terminal" {
		return Claims{}, errors.New("capability role or scope is invalid")
	}
	if claims.IssuedAt > now+30 || claims.ExpiresAt <= now || claims.ExpiresAt <= claims.IssuedAt {
		return Claims{}, errors.New("capability is expired or not yet valid")
	}
	return claims, nil
}

func ParsePrivateKeyPEM(contents []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}
	return privateKey, nil
}

func ParsePublicKeyPEM(contents []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	publicKey, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return publicKey, nil
}
