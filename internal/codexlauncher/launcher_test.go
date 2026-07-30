package codexlauncher

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCommandStartsFreshWithoutSession(t *testing.T) {
	home := t.TempDir()
	got, err := Command("/usr/local/bin/codex", home, []string{"--no-alt-screen"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/local/bin/codex", "--no-alt-screen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func TestCommandResumesLatestDurableSession(t *testing.T) {
	home := t.TempDir()
	session := filepath.Join(home, "sessions", "2026", "07", "30", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(session), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("session-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Command("/usr/local/bin/codex", home, []string{"-c", `forced_login_method="chatgpt"`})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/local/bin/codex", "-c", `forced_login_method="chatgpt"`, "resume", "--last"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}
