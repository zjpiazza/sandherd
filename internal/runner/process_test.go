package runner

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPTYHelperProcess(t *testing.T) {
	if os.Getenv("SANDHERD_PTY_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.Write([]byte("snowman=\xe2\x98\x83 binary=\x00\xff\r\n"))
	input := make([]byte, 128)
	count, _ := os.Stdin.Read(input)
	_, _ = os.Stdout.Write([]byte("received="))
	_, _ = os.Stdout.Write(input[:count])
	os.Exit(0)
}

func TestAgentProcessPTYCarriesBinaryUTF8InputAndExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var outputMu sync.Mutex
	var output bytes.Buffer
	exitChannel := make(chan processExit, 1)
	process := newAgentProcess(
		[]string{executable, "-test.run=^TestPTYHelperProcess$"},
		"",
		append(os.Environ(), "SANDHERD_PTY_HELPER=1"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(data []byte) {
			outputMu.Lock()
			_, _ = output.Write(data)
			outputMu.Unlock()
		},
		func(exit processExit) { exitChannel <- exit },
	)
	if err := process.start(TerminalSize{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("start PTY: %v", err)
	}
	if err := process.resize(TerminalSize{Columns: 100, Rows: 30}); err != nil {
		t.Fatalf("resize PTY: %v", err)
	}
	if err := process.write([]byte("hello\n")); err != nil {
		t.Fatalf("write PTY: %v", err)
	}
	select {
	case exit := <-exitChannel:
		if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Signal != "" {
			t.Fatalf("exit = %#v, want code 0", exit)
		}
	case <-time.After(5 * time.Second):
		process.terminate(100 * time.Millisecond)
		t.Fatal("helper process did not exit")
	}
	outputMu.Lock()
	got := append([]byte(nil), output.Bytes()...)
	outputMu.Unlock()
	if !bytes.Contains(got, []byte("snowman=\xe2\x98\x83")) || !bytes.Contains(got, []byte{0x00, 0xff}) {
		t.Fatalf("PTY output did not preserve UTF-8 and binary bytes: %q", got)
	}
	if !bytes.Contains(got, []byte("hello")) || !bytes.Contains(got, []byte("received=")) {
		t.Fatalf("PTY output did not carry input: %q", got)
	}
}

func TestAgentProcessSurvivesAttachmentDetach(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newHub("00000000-0000-4000-8000-000000000001", 1024, 32, 8, time.Minute)
	t.Cleanup(h.close)
	exitChannel := make(chan processExit, 1)
	process := newAgentProcess([]string{"/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"}, "", os.Environ(), logger, h.publishOutput, func(exit processExit) {
		h.publishExit(exit)
		exitChannel <- exit
	})
	if err := process.start(TerminalSize{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("start process: %v", err)
	}
	h.setProcess(process)
	sub := attachForTest(t, h, "control", nil, false, Permissions{Observe: true, Control: true})
	h.detach(sub)
	pid, _, _ := process.snapshot()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("process after detach: %v", err)
	}
	process.terminate(2 * time.Second)
	select {
	case <-exitChannel:
	case <-time.After(3 * time.Second):
		t.Fatal("process did not terminate")
	}
}

func TestAgentProcessForcesTerminationAfterGrace(t *testing.T) {
	exitChannel := make(chan processExit, 1)
	ready := make(chan struct{})
	var readyOnce sync.Once
	process := newAgentProcess(
		[]string{"/bin/sh", "-c", "trap '' TERM; echo ready; while :; do sleep 1; done"},
		"", os.Environ(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(data []byte) {
			if bytes.Contains(data, []byte("ready")) {
				readyOnce.Do(func() { close(ready) })
			}
		}, func(exit processExit) { exitChannel <- exit },
	)
	if err := process.start(TerminalSize{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("start process: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		process.terminate(50 * time.Millisecond)
		t.Fatal("process did not become ready")
	}
	started := time.Now()
	process.terminate(50 * time.Millisecond)
	if elapsed := time.Since(started); elapsed < 45*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("termination elapsed = %v, want grace then prompt kill", elapsed)
	}
	select {
	case exit := <-exitChannel:
		if exit.Signal != "SIGKILL" && !strings.Contains(exit.Signal, "KILL") {
			t.Fatalf("forced exit signal = %q, want SIGKILL", exit.Signal)
		}
	default:
		t.Fatal("forced process exit was not reported")
	}
}
