package runner

import (
	"io"
	"reflect"
	"testing"
)

func TestParseConfigLoadsTrustedAdapterCommandFromEnvironment(t *testing.T) {
	t.Setenv("SANDHERD_AGENT_COMMAND_JSON", `["/bin/sh","-l"]`)
	t.Setenv("SANDHERD_AGENT_HEALTH_CHECK_JSON", `["/bin/sh","-c","exit 0"]`)
	configuration, showVersion, err := parseConfig([]string{
		"--agent-id=00000000-0000-4000-8000-000000000001",
		"--capability-public-key-file=/tmp/public.pem",
	}, io.Discard)
	if err != nil || showVersion {
		t.Fatalf("parseConfig() error=%v showVersion=%v", err, showVersion)
	}
	if !reflect.DeepEqual(configuration.command, []string{"/bin/sh", "-l"}) {
		t.Fatalf("command = %#v", configuration.command)
	}
}

func TestParseConfigRejectsUnsafeAdapterCommand(t *testing.T) {
	for _, command := range []string{`not-json`, `["relative"]`, `[]`} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("SANDHERD_AGENT_COMMAND_JSON", command)
			t.Setenv("SANDHERD_AGENT_HEALTH_CHECK_JSON", `["/bin/sh","-c","exit 0"]`)
			if _, _, err := parseConfig([]string{
				"--agent-id=00000000-0000-4000-8000-000000000001",
				"--capability-public-key-file=/tmp/public.pem",
			}, io.Discard); err == nil {
				t.Fatal("unsafe adapter command was accepted")
			}
		})
	}
}

func TestParseConfigRejectsUnsafeAdapterHealthCheck(t *testing.T) {
	t.Setenv("SANDHERD_AGENT_COMMAND_JSON", `["/bin/sh"]`)
	for _, command := range []string{`not-json`, `["relative"]`, `[]`} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("SANDHERD_AGENT_HEALTH_CHECK_JSON", command)
			if _, _, err := parseConfig([]string{
				"--agent-id=00000000-0000-4000-8000-000000000001",
				"--capability-public-key-file=/tmp/public.pem",
			}, io.Discard); err == nil {
				t.Fatal("unsafe adapter health check was accepted")
			}
		})
	}
}

func TestRunAdapterHealthCheckRedactsFailure(t *testing.T) {
	err := runAdapterHealthCheck([]string{"/bin/sh", "-c", "echo sensitive-output >&2; exit 1"}, t.TempDir(), nil)
	if err == nil || err.Error() != "adapter health check failed" {
		t.Fatalf("error = %v", err)
	}
}
