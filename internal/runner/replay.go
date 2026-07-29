package runner

type replayBuffer struct {
	maxBytes  int
	maxFrames int
	bytes     int
	frames    []Frame
	latest    uint64
}

func newReplayBuffer(maxBytes, maxFrames int) *replayBuffer {
	return &replayBuffer{maxBytes: maxBytes, maxFrames: maxFrames}
}

func (b *replayBuffer) append(data []byte) Frame {
	b.latest++
	sequence := b.latest
	copyOfData := append([]byte(nil), data...)
	frame := Frame{Type: "output", Sequence: &sequence, Data: copyOfData}
	b.frames = append(b.frames, frame)
	b.bytes += len(copyOfData)

	for len(b.frames) > 0 && (b.bytes > b.maxBytes || len(b.frames) > b.maxFrames) {
		b.bytes -= len(b.frames[0].Data)
		b.frames[0].Data = nil
		b.frames = b.frames[1:]
	}
	return frame
}

func (b *replayBuffer) bounds() (earliest, latest uint64) {
	if len(b.frames) == 0 {
		return b.latest, b.latest
	}
	return *b.frames[0].Sequence, b.latest
}

func (b *replayBuffer) after(cursor uint64) (frames []Frame, gap bool, valid bool) {
	if cursor > b.latest {
		return nil, false, false
	}
	if len(b.frames) == 0 {
		return nil, cursor < b.latest, true
	}
	earliest := *b.frames[0].Sequence
	gap = cursor+1 < earliest
	for _, frame := range b.frames {
		if *frame.Sequence > cursor {
			frames = append(frames, frame)
		}
	}
	return frames, gap, true
}
