package runtimeadapter

import (
	"errors"
	"reflect"
	"testing"
)

func testConfig() Config {
	return Config{Version: 1, Adapters: []Definition{
		{
			ID: "shell", DisplayName: "Shell", Version: "1",
			Capabilities: []Capability{CapabilityInteractive},
			Profiles: []Profile{
				{SandboxProfile: "standard", CredentialMode: CredentialNone, WarmPool: "sandherd-standard", Command: []string{"/bin/bash", "--noprofile", "--norc"}, HealthCheck: []string{"/bin/bash", "--version"}},
				{SandboxProfile: "standard", CredentialProfile: "personal", CredentialMode: CredentialMutable, WarmPool: "sandherd-standard-personal", Command: []string{"/bin/bash"}, HealthCheck: []string{"/bin/bash", "--version"}},
			},
		},
		{
			ID: "shell_minimal", DisplayName: "Minimal shell", Version: "1",
			Capabilities: []Capability{CapabilityInteractive, CapabilityHeadless},
			Profiles:     []Profile{{SandboxProfile: "standard", CredentialMode: CredentialNone, WarmPool: "sandherd-standard", Command: []string{"/bin/sh"}, HealthCheck: []string{"/bin/sh", "-c", "exit 0"}}},
		},
	}}
}

func TestRegistryResolvesExactApprovedProfile(t *testing.T) {
	registry, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := registry.Resolve("shell", "standard", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.WarmPool != "sandherd-standard-personal" || runtime.CredentialMode != CredentialMutable || runtime.CommandJSON() != `["/bin/bash"]` || runtime.HealthCheckJSON() != `["/bin/bash","--version"]` {
		t.Fatalf("resolved runtime = %#v", runtime)
	}
	runtime.Command[0] = "/tampered"
	runtime.HealthCheck[0] = "/tampered"
	again, _ := registry.Resolve("shell", "standard", "personal")
	if again.Command[0] != "/bin/bash" || again.HealthCheck[0] != "/bin/bash" {
		t.Fatalf("registry commands were mutated: %#v %#v", again.Command, again.HealthCheck)
	}
	if _, err := registry.Resolve("missing", "standard", ""); !errors.Is(err, ErrAdapterNotFound) {
		t.Fatalf("missing adapter error = %v", err)
	}
	if _, err := registry.Resolve("shell", "restricted", ""); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("missing profile error = %v", err)
	}
}

func TestRegistryListsOnlySafeAdapterMetadata(t *testing.T) {
	registry, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	list := registry.List()
	if got := []string{list.Items[0].ID, list.Items[1].ID}; !reflect.DeepEqual(got, []string{"shell", "shell_minimal"}) {
		t.Fatalf("adapter order = %#v", got)
	}
	if list.Items[0].DisplayName != "Shell" || len(list.Items[0].Capabilities) != 1 {
		t.Fatalf("descriptor = %#v", list.Items[0])
	}
}

func TestRegistryRejectsUnsafeOrAmbiguousDefinitions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "unknown version", mutate: func(config *Config) { config.Version = 2 }},
		{name: "invalid ID", mutate: func(config *Config) { config.Adapters[0].ID = "../shell" }},
		{name: "relative executable", mutate: func(config *Config) { config.Adapters[0].Profiles[0].Command[0] = "bash" }},
		{name: "relative health check", mutate: func(config *Config) { config.Adapters[0].Profiles[0].HealthCheck[0] = "bash" }},
		{name: "credential mismatch", mutate: func(config *Config) { config.Adapters[0].Profiles[0].CredentialMode = CredentialMutable }},
		{name: "duplicate binding", mutate: func(config *Config) {
			config.Adapters[0].Profiles = append(config.Adapters[0].Profiles, config.Adapters[0].Profiles[0])
		}},
		{name: "unknown capability", mutate: func(config *Config) { config.Adapters[0].Capabilities[0] = "telepathy" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			test.mutate(&config)
			if _, err := New(config); err == nil {
				t.Fatal("invalid adapter configuration was accepted")
			}
		})
	}
}

func TestStructurallyDifferentAdapterFixturesShareTheRegistryContract(t *testing.T) {
	fixtures := Config{Version: 1, Adapters: []Definition{
		{
			ID: "fake", DisplayName: "Contract fake", Version: "test",
			Capabilities: []Capability{CapabilityInteractive, CapabilityHeadless},
			Profiles: []Profile{{SandboxProfile: "standard", CredentialMode: CredentialNone, WarmPool: "fake-pool",
				Command: []string{"/usr/local/bin/fake-agent"}, HealthCheck: []string{"/usr/local/bin/fake-agent", "health"}}},
		},
		{
			ID: "codex", DisplayName: "Codex fixture", Version: "fixture-v1",
			Capabilities: []Capability{CapabilityInteractive, CapabilityHeadless, CapabilitySessionResume, CapabilityMCP, CapabilitySubscriptionAuth},
			Profiles: []Profile{{SandboxProfile: "standard", CredentialProfile: "subscription", CredentialMode: CredentialMutable, WarmPool: "codex-subscription",
				Command: []string{"/usr/local/bin/codex"}, HealthCheck: []string{"/usr/local/bin/codex", "--version"}}},
		},
		{
			ID: "claude-code", DisplayName: "Claude Code fixture", Version: "fixture-v1",
			Capabilities: []Capability{CapabilityInteractive, CapabilityHeadless, CapabilitySessionResume, CapabilityMCP},
			Profiles: []Profile{{SandboxProfile: "restricted", CredentialProfile: "api-key", CredentialMode: CredentialImmutable, WarmPool: "claude-api",
				Command: []string{"/usr/local/bin/claude", "--permission-mode", "default"}, HealthCheck: []string{"/usr/local/bin/claude", "--version"}}},
		},
	}}
	registry, err := New(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id, sandbox, credential string
		mode                    CredentialMode
	}{
		{id: "fake", sandbox: "standard", mode: CredentialNone},
		{id: "codex", sandbox: "standard", credential: "subscription", mode: CredentialMutable},
		{id: "claude-code", sandbox: "restricted", credential: "api-key", mode: CredentialImmutable},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			resolved, err := registry.Resolve(test.id, test.sandbox, test.credential)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.CredentialMode != test.mode || len(resolved.Command) == 0 || len(resolved.HealthCheck) == 0 {
				t.Fatalf("resolved contract = %#v", resolved)
			}
		})
	}
}
