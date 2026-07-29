package runner

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	mu      sync.Mutex
	input   bytes.Buffer
	sizes   []TerminalSize
	started time.Time
}

type blockingProcess struct {
	*fakeProcess
	started chan struct{}
	release chan struct{}
}

func (p *blockingProcess) write(data []byte) error {
	close(p.started)
	<-p.release
	return p.fakeProcess.write(data)
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{started: time.Now().UTC()}
}

func (p *fakeProcess) write(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.input.Write(data)
	return nil
}

func (p *fakeProcess) resize(size TerminalSize) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes = append(p.sizes, size)
	return nil
}

func (p *fakeProcess) snapshot() (int, time.Time, *processExit) {
	return 42, p.started, nil
}

func testHub(t *testing.T, replayBytes, subscriberBuffer int, lease time.Duration) (*hub, *fakeProcess) {
	t.Helper()
	h := newHub("00000000-0000-4000-8000-000000000001", replayBytes, 32, subscriberBuffer, lease)
	p := newFakeProcess()
	h.setProcess(p)
	t.Cleanup(h.close)
	return h, p
}

func attachForTest(t *testing.T, h *hub, role string, after *uint64, takeover bool, permissions Permissions) *subscription {
	t.Helper()
	sub, err := h.attach(attachRequest{
		Role: role, AfterSequence: after, Takeover: takeover,
		TerminalSize: TerminalSize{Columns: 80, Rows: 24}, Permissions: permissions,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	return sub
}

func TestHubReplayThenLiveWithoutDuplication(t *testing.T) {
	h, _ := testHub(t, 1024, 8, time.Minute)
	h.publishOutput([]byte("one"))
	h.publishOutput([]byte("two"))
	cursor := uint64(1)
	sub := attachForTest(t, h, "observe", &cursor, false, Permissions{Observe: true})

	if len(sub.initial) != 2 || sub.initial[0].Type != "attached" || *sub.initial[1].Sequence != 2 {
		t.Fatalf("initial frames = %#v, want attached then output 2", sub.initial)
	}
	h.publishOutput([]byte("three"))
	select {
	case frame := <-sub.queue:
		if frame.Type != "output" || *frame.Sequence != 3 {
			t.Fatalf("live frame = %#v, want output 3", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live output")
	}
}

func TestHubReportsExpiredReplayCursor(t *testing.T) {
	h, _ := testHub(t, 3, 8, time.Minute)
	h.publishOutput([]byte("aa"))
	h.publishOutput([]byte("bb"))
	cursor := uint64(0)
	sub := attachForTest(t, h, "observe", &cursor, false, Permissions{Observe: true})
	if len(sub.initial) != 3 || sub.initial[1].Type != "replay_gap" || *sub.initial[2].Sequence != 2 {
		t.Fatalf("initial frames = %#v, want attached, replay gap, output 2", sub.initial)
	}
}

func TestHubControllerTakeoverIsAtomic(t *testing.T) {
	h, process := testHub(t, 1024, 8, time.Minute)
	control := Permissions{Observe: true, Control: true}
	first := attachForTest(t, h, "control", nil, false, control)
	observer := attachForTest(t, h, "observe", nil, false, Permissions{Observe: true})
	if _, err := h.takeover(observer); errorCode(err) != "forbidden_role" {
		t.Fatalf("observe-only takeover error = %v, want forbidden_role", err)
	}
	if err := h.writeInput(observer, first.leaseID, []byte("forbidden")); errorCode(err) != "invalid_controller_lease" {
		t.Fatalf("observer input error = %v, want invalid_controller_lease", err)
	}
	if err := h.resize(observer, first.leaseID, TerminalSize{Columns: 100, Rows: 40}); errorCode(err) != "invalid_controller_lease" {
		t.Fatalf("observer resize error = %v, want invalid_controller_lease", err)
	}
	if _, err := h.attach(attachRequest{Role: "control", TerminalSize: TerminalSize{Columns: 80, Rows: 24}, Permissions: control}); errorCode(err) != "controller_busy" {
		t.Fatalf("second controller error = %v, want controller_busy", err)
	}

	second := attachForTest(t, h, "control", nil, true, control)
	select {
	case frame := <-first.queue:
		if frame.Type != "controller_revoked" || frame.Reason != "takeover" {
			t.Fatalf("revocation = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("first controller was not revoked")
	}
	if err := h.writeInput(first, first.initial[0].LeaseID, []byte("stale")); errorCode(err) != "invalid_controller_lease" {
		t.Fatalf("stale input error = %v, want invalid_controller_lease", err)
	}
	if err := h.writeInput(second, second.leaseID, []byte("accepted")); err != nil {
		t.Fatalf("new controller input: %v", err)
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	if got := process.input.String(); got != "accepted" {
		t.Fatalf("process input = %q, want accepted", got)
	}
}

func TestHubTakeoverWaitsForInFlightControllerWrite(t *testing.T) {
	h := newHub("00000000-0000-4000-8000-000000000001", 1024, 32, 8, time.Minute)
	process := &blockingProcess{fakeProcess: newFakeProcess(), started: make(chan struct{}), release: make(chan struct{})}
	h.setProcess(process)
	t.Cleanup(h.close)
	permissions := Permissions{Observe: true, Control: true}
	first := attachForTest(t, h, "control", nil, false, permissions)

	writeDone := make(chan error, 1)
	go func() { writeDone <- h.writeInput(first, first.leaseID, []byte("first")) }()
	select {
	case <-process.started:
	case <-time.After(time.Second):
		t.Fatal("controller write did not start")
	}
	takeoverDone := make(chan *subscription, 1)
	go func() {
		sub, _ := h.attach(attachRequest{
			Role: "control", Takeover: true, TerminalSize: TerminalSize{Columns: 80, Rows: 24}, Permissions: permissions,
		})
		takeoverDone <- sub
	}()
	select {
	case <-takeoverDone:
		t.Fatal("takeover completed while the prior controller was still writing")
	case <-time.After(25 * time.Millisecond):
	}
	close(process.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("first write: %v", err)
	}
	select {
	case second := <-takeoverDone:
		if second == nil || second.leaseID == first.leaseID {
			t.Fatalf("takeover subscription = %#v", second)
		}
	case <-time.After(time.Second):
		t.Fatal("takeover did not complete after the write")
	}
}

func TestHubExpiresControllerLease(t *testing.T) {
	h, _ := testHub(t, 1024, 8, 25*time.Millisecond)
	go h.runLeaseReaper()
	sub := attachForTest(t, h, "control", nil, false, Permissions{Observe: true, Control: true})
	select {
	case frame := <-sub.queue:
		if frame.Type != "controller_revoked" || frame.Reason != "lease_expired" {
			t.Fatalf("lease expiry frame = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("controller lease did not expire")
	}
}

func TestHubDisconnectDoesNotAffectProcessAndSlowConsumerIsBounded(t *testing.T) {
	h, process := testHub(t, 1024, 1, time.Minute)
	sub := attachForTest(t, h, "observe", nil, false, Permissions{Observe: true})
	h.detach(sub)
	if pid, _, _ := process.snapshot(); pid != 42 {
		t.Fatalf("process PID after detach = %d, want 42", pid)
	}

	slow := attachForTest(t, h, "observe", nil, false, Permissions{Observe: true})
	h.publishOutput([]byte("one"))
	h.publishOutput([]byte("two"))
	select {
	case <-slow.done:
		if reason := slow.closeReason(); reason.Code != "slow_consumer" || !reason.Retryable {
			t.Fatalf("close reason = %#v", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("slow consumer remained attached")
	}
}

func TestHubDeliversExitOnceToLiveAndReconnect(t *testing.T) {
	h, _ := testHub(t, 1024, 8, time.Minute)
	live := attachForTest(t, h, "observe", nil, false, Permissions{Observe: true})
	code := 7
	exit := processExit{ExitCode: &code, FinishedAt: time.Now().UTC()}
	h.publishExit(exit)
	h.publishExit(exit)
	select {
	case frame := <-live.queue:
		if frame.Type != "exit" || frame.ExitCode == nil || *frame.ExitCode != 7 {
			t.Fatalf("exit frame = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("live attachment did not receive exit")
	}
	select {
	case duplicate := <-live.queue:
		t.Fatalf("duplicate exit = %#v", duplicate)
	default:
	}
	reconnected := attachForTest(t, h, "observe", nil, false, Permissions{Observe: true})
	count := 0
	for _, frame := range reconnected.initial {
		if frame.Type == "exit" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reconnect exit count = %d, want 1", count)
	}
}
