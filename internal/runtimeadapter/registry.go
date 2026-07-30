// Package runtimeadapter defines the operator-installed agent adapter contract.
package runtimeadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxConfigBytes = 1024 * 1024

var (
	ErrAdapterNotFound = errors.New("agent adapter is not installed")
	ErrProfileNotFound = errors.New("agent adapter profile is not installed")
	identifierPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// CredentialMode describes how the selected runtime profile obtains agent
// credentials. Credential contents and Kubernetes resource names are never
// represented in this configuration.
type CredentialMode string

const (
	CredentialNone             CredentialMode = "none"
	CredentialImmutable        CredentialMode = "immutable"
	CredentialMutable          CredentialMode = "mutable"
	CredentialWorkloadIdentity CredentialMode = "workload-identity"
)

// Capability is an adapter feature that clients can discover without learning
// internal launch or credential details.
type Capability string

const (
	CapabilityInteractive      Capability = "interactive"
	CapabilityHeadless         Capability = "headless"
	CapabilitySessionResume    Capability = "session-resume"
	CapabilityMCP              Capability = "mcp"
	CapabilityACP              Capability = "acp"
	CapabilitySubscriptionAuth Capability = "subscription-auth"
)

type Config struct {
	Version  int          `json:"version"`
	Adapters []Definition `json:"adapters"`
}

type Definition struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"displayName"`
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
	Profiles     []Profile    `json:"profiles"`
}

type Profile struct {
	SandboxProfile    string         `json:"sandboxProfile"`
	CredentialProfile string         `json:"credentialProfile,omitempty"`
	CredentialMode    CredentialMode `json:"credentialMode"`
	WarmPool          string         `json:"warmPool"`
	Command           []string       `json:"command"`
	HealthCheck       []string       `json:"healthCheck"`
}

// Descriptor is the safe public view of an installed adapter.
type Descriptor struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"displayName"`
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

type List struct {
	Items []Descriptor `json:"items"`
}

// Runtime is the trusted launch decision for one Agent generation.
type Runtime struct {
	AdapterID         string
	AdapterVersion    string
	SandboxProfile    string
	CredentialProfile string
	CredentialMode    CredentialMode
	WarmPool          string
	Command           []string
	HealthCheck       []string
}

func (r Runtime) CommandJSON() string {
	encoded, _ := json.Marshal(r.Command)
	return string(encoded)
}

func (r Runtime) HealthCheckJSON() string {
	encoded, _ := json.Marshal(r.HealthCheck)
	return string(encoded)
}

type Registry struct {
	definitions map[string]Definition
}

func Load(path string) (*Registry, error) {
	if path == "" {
		return nil, fmt.Errorf("adapter configuration file is required")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("open adapter configuration: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read adapter configuration: %w", err)
	}
	if len(contents) > maxConfigBytes {
		return nil, fmt.Errorf("adapter configuration exceeds %d bytes", maxConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode adapter configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("adapter configuration must contain one JSON value")
	}
	return New(config)
}

func New(config Config) (*Registry, error) {
	if config.Version != 1 {
		return nil, fmt.Errorf("adapter configuration version must be 1")
	}
	if len(config.Adapters) == 0 || len(config.Adapters) > 64 {
		return nil, fmt.Errorf("adapter configuration must contain between 1 and 64 adapters")
	}
	registry := &Registry{definitions: make(map[string]Definition, len(config.Adapters))}
	for index := range config.Adapters {
		definition := config.Adapters[index]
		if err := validateDefinition(definition); err != nil {
			return nil, fmt.Errorf("adapter %d: %w", index, err)
		}
		if _, exists := registry.definitions[definition.ID]; exists {
			return nil, fmt.Errorf("adapter IDs must be unique")
		}
		definition.Capabilities = append([]Capability(nil), definition.Capabilities...)
		definition.Profiles = append([]Profile(nil), definition.Profiles...)
		for profileIndex := range definition.Profiles {
			definition.Profiles[profileIndex].Command = append([]string(nil), definition.Profiles[profileIndex].Command...)
			definition.Profiles[profileIndex].HealthCheck = append([]string(nil), definition.Profiles[profileIndex].HealthCheck...)
		}
		registry.definitions[definition.ID] = definition
	}
	return registry, nil
}

func (r *Registry) Resolve(adapterID, sandboxProfile, credentialProfile string) (Runtime, error) {
	definition, exists := r.definitions[adapterID]
	if !exists {
		return Runtime{}, ErrAdapterNotFound
	}
	for _, profile := range definition.Profiles {
		if profile.SandboxProfile == sandboxProfile && profile.CredentialProfile == credentialProfile {
			return Runtime{
				AdapterID: definition.ID, AdapterVersion: definition.Version,
				SandboxProfile: profile.SandboxProfile, CredentialProfile: profile.CredentialProfile,
				CredentialMode: profile.CredentialMode, WarmPool: profile.WarmPool,
				Command:     append([]string(nil), profile.Command...),
				HealthCheck: append([]string(nil), profile.HealthCheck...),
			}, nil
		}
	}
	return Runtime{}, ErrProfileNotFound
}

func (r *Registry) List() List {
	result := List{Items: make([]Descriptor, 0, len(r.definitions))}
	for _, definition := range r.definitions {
		result.Items = append(result.Items, Descriptor{
			ID: definition.ID, DisplayName: definition.DisplayName, Version: definition.Version,
			Capabilities: append([]Capability(nil), definition.Capabilities...),
		})
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].ID < result.Items[j].ID })
	return result
}

func validateDefinition(definition Definition) error {
	if !validIdentifier(definition.ID) {
		return fmt.Errorf("ID is invalid")
	}
	if strings.TrimSpace(definition.DisplayName) != definition.DisplayName || len(definition.DisplayName) < 1 || len(definition.DisplayName) > 128 {
		return fmt.Errorf("displayName is invalid")
	}
	if strings.TrimSpace(definition.Version) != definition.Version || len(definition.Version) < 1 || len(definition.Version) > 128 || strings.IndexFunc(definition.Version, invalidControlCharacter) >= 0 {
		return fmt.Errorf("version is invalid")
	}
	if len(definition.Capabilities) == 0 || len(definition.Capabilities) > 16 {
		return fmt.Errorf("capabilities must contain between 1 and 16 values")
	}
	seenCapabilities := make(map[Capability]struct{}, len(definition.Capabilities))
	for _, capability := range definition.Capabilities {
		if !validCapability(capability) {
			return fmt.Errorf("capability %q is invalid", capability)
		}
		if _, exists := seenCapabilities[capability]; exists {
			return fmt.Errorf("capability %q is repeated", capability)
		}
		seenCapabilities[capability] = struct{}{}
	}
	if len(definition.Profiles) == 0 || len(definition.Profiles) > 128 {
		return fmt.Errorf("profiles must contain between 1 and 128 values")
	}
	seenProfiles := make(map[string]struct{}, len(definition.Profiles))
	for index, profile := range definition.Profiles {
		if err := validateProfile(profile); err != nil {
			return fmt.Errorf("profile %d: %w", index, err)
		}
		key := profile.SandboxProfile + "\x00" + profile.CredentialProfile
		if _, exists := seenProfiles[key]; exists {
			return fmt.Errorf("profile combination is repeated")
		}
		seenProfiles[key] = struct{}{}
	}
	return nil
}

func validateProfile(profile Profile) error {
	if !validIdentifier(profile.SandboxProfile) {
		return fmt.Errorf("sandboxProfile is invalid")
	}
	if profile.CredentialProfile != "" && !validIdentifier(profile.CredentialProfile) {
		return fmt.Errorf("credentialProfile is invalid")
	}
	if profile.CredentialProfile == "" && profile.CredentialMode != CredentialNone {
		return fmt.Errorf("credentialMode must be none without a credentialProfile")
	}
	if profile.CredentialProfile != "" && profile.CredentialMode == CredentialNone {
		return fmt.Errorf("credentialMode must describe a configured credentialProfile")
	}
	if !validCredentialMode(profile.CredentialMode) {
		return fmt.Errorf("credentialMode is invalid")
	}
	if strings.TrimSpace(profile.WarmPool) != profile.WarmPool || len(profile.WarmPool) < 1 || len(profile.WarmPool) > 253 || strings.IndexFunc(profile.WarmPool, invalidControlCharacter) >= 0 {
		return fmt.Errorf("warmPool is invalid")
	}
	if err := validateCommand(profile.Command); err != nil {
		return fmt.Errorf("command %w", err)
	}
	if err := validateCommand(profile.HealthCheck); err != nil {
		return fmt.Errorf("healthCheck %w", err)
	}
	return nil
}

func validateCommand(command []string) error {
	if len(command) == 0 || len(command) > 64 || !filepath.IsAbs(command[0]) {
		return fmt.Errorf("must contain an absolute executable and at most 64 arguments")
	}
	for _, argument := range command {
		if len(argument) > 4096 || strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("contains an invalid argument")
		}
	}
	return nil
}

func validIdentifier(value string) bool { return identifierPattern.MatchString(value) }

func validCredentialMode(mode CredentialMode) bool {
	switch mode {
	case CredentialNone, CredentialImmutable, CredentialMutable, CredentialWorkloadIdentity:
		return true
	default:
		return false
	}
}

func validCapability(capability Capability) bool {
	switch capability {
	case CapabilityInteractive, CapabilityHeadless, CapabilitySessionResume, CapabilityMCP, CapabilityACP, CapabilitySubscriptionAuth:
		return true
	default:
		return false
	}
}

func invalidControlCharacter(value rune) bool { return value < 0x20 || value == 0x7f }
