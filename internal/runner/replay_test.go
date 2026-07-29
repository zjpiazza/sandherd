package runner

import (
	"bytes"
	"testing"
)

func TestReplayBufferRolloverAndGap(t *testing.T) {
	buffer := newReplayBuffer(4, 3)
	first := buffer.append([]byte("aa"))
	second := buffer.append([]byte("bb"))
	third := buffer.append([]byte("cc"))

	if *first.Sequence != 1 || *second.Sequence != 2 || *third.Sequence != 3 {
		t.Fatalf("sequences = %d, %d, %d; want 1, 2, 3", *first.Sequence, *second.Sequence, *third.Sequence)
	}
	earliest, latest := buffer.bounds()
	if earliest != 2 || latest != 3 {
		t.Fatalf("bounds = %d..%d, want 2..3", earliest, latest)
	}
	frames, gap, valid := buffer.after(0)
	if !valid || !gap || len(frames) != 2 {
		t.Fatalf("after(0) = %d frames, gap=%v, valid=%v; want 2, true, true", len(frames), gap, valid)
	}
	if !bytes.Equal(frames[0].Data, []byte("bb")) || !bytes.Equal(frames[1].Data, []byte("cc")) {
		t.Fatalf("replay data = %q, %q", frames[0].Data, frames[1].Data)
	}
	if _, _, valid := buffer.after(4); valid {
		t.Fatal("after(4) is valid for latest sequence 3")
	}
}

func TestReplayBufferBoundsOversizedFrame(t *testing.T) {
	buffer := newReplayBuffer(2, 2)
	buffer.append([]byte("oversized"))
	earliest, latest := buffer.bounds()
	if earliest != 1 || latest != 1 {
		t.Fatalf("empty retained bounds = %d..%d, want 1..1", earliest, latest)
	}
	frames, gap, valid := buffer.after(0)
	if !valid || !gap || len(frames) != 0 {
		t.Fatalf("after discarded frame = %d frames, gap=%v, valid=%v", len(frames), gap, valid)
	}
}

func TestDecodeFrameRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"type":"ping","nonce":"x","unknown":true}`,
		`{"type":"ping","nonce":"x"} {"type":"pong","nonce":"x"}`,
		`{"type":"ack"}`,
		`{"type":"output","sequence":1,"data":"eA=="}`,
	} {
		if _, err := decodeFrame([]byte(input)); err == nil {
			t.Fatalf("decodeFrame(%q) succeeded, want error", input)
		}
	}
}
