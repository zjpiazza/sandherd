package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const (
	Protocol        = "sandherd.terminal.v1alpha1"
	ProtocolVersion = "v1alpha1"
)

type TerminalSize struct {
	Columns     uint16 `json:"columns"`
	Rows        uint16 `json:"rows"`
	PixelWidth  uint16 `json:"pixelWidth,omitempty"`
	PixelHeight uint16 `json:"pixelHeight,omitempty"`
}

func (s TerminalSize) Validate() error {
	if s.Columns == 0 || s.Rows == 0 || s.Columns > 1000 || s.Rows > 1000 {
		return fmt.Errorf("terminal columns and rows must be between 1 and 1000")
	}
	return nil
}

// Frame is the v1alpha1 terminal envelope. Fields are populated according to Type.
// Data contains base64 on the wire and raw bytes in memory.
type Frame struct {
	Type                   string        `json:"type"`
	ProtocolVersion        string        `json:"protocolVersion,omitempty"`
	Role                   string        `json:"role,omitempty"`
	AfterSequence          *uint64       `json:"afterSequence,omitempty"`
	Takeover               bool          `json:"takeover,omitempty"`
	TerminalSize           *TerminalSize `json:"terminalSize,omitempty"`
	AttachmentID           string        `json:"attachmentId,omitempty"`
	AgentID                string        `json:"agentId,omitempty"`
	LeaseID                string        `json:"leaseId,omitempty"`
	RunnerGeneration       string        `json:"runnerGeneration,omitempty"`
	EarliestSequence       *uint64       `json:"earliestSequence,omitempty"`
	LatestSequence         *uint64       `json:"latestSequence,omitempty"`
	ProcessState           string        `json:"processState,omitempty"`
	Sequence               *uint64       `json:"sequence,omitempty"`
	Data                   []byte        `json:"data,omitempty"`
	RequestedAfterSequence *uint64       `json:"requestedAfterSequence,omitempty"`
	Reason                 string        `json:"reason,omitempty"`
	ExitCode               *int          `json:"exitCode,omitempty"`
	Signal                 string        `json:"signal,omitempty"`
	FinishedAt             string        `json:"finishedAt,omitempty"`
	Code                   string        `json:"code,omitempty"`
	Message                string        `json:"message,omitempty"`
	RequestID              string        `json:"requestId,omitempty"`
	Retryable              bool          `json:"retryable,omitempty"`
	Nonce                  string        `json:"nonce,omitempty"`
}

func decodeFrame(data []byte) (Frame, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Frame{}, fmt.Errorf("decode terminal frame: %w", err)
	}
	var frameType string
	if rawType, ok := object["type"]; ok {
		if err := json.Unmarshal(rawType, &frameType); err != nil {
			return Frame{}, fmt.Errorf("decode terminal frame type: %w", err)
		}
	}
	shape, ok := clientFrameShapes[frameType]
	if !ok {
		return Frame{}, fmt.Errorf("decode terminal frame: unsupported client frame type %q", frameType)
	}
	for name := range object {
		if !shape.allowed[name] {
			return Frame{}, fmt.Errorf("decode terminal frame: field %q is not allowed on %s", name, frameType)
		}
	}
	for name := range shape.required {
		if _, ok := object[name]; !ok {
			return Frame{}, fmt.Errorf("decode terminal frame: %s requires field %q", frameType, name)
		}
	}

	var frame Frame
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		return Frame{}, fmt.Errorf("decode terminal frame: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Frame{}, fmt.Errorf("decode terminal frame: trailing JSON value")
	}
	return frame, nil
}

type frameShape struct {
	allowed  map[string]bool
	required map[string]bool
}

func shape(required []string, optional ...string) frameShape {
	result := frameShape{allowed: make(map[string]bool), required: make(map[string]bool)}
	for _, name := range required {
		result.allowed[name] = true
		result.required[name] = true
	}
	for _, name := range optional {
		result.allowed[name] = true
	}
	return result
}

var clientFrameShapes = map[string]frameShape{
	"attach":   shape([]string{"type", "protocolVersion", "role", "terminalSize"}, "afterSequence", "takeover"),
	"input":    shape([]string{"type", "leaseId", "data"}),
	"resize":   shape([]string{"type", "leaseId", "terminalSize"}),
	"ack":      shape([]string{"type", "sequence"}),
	"takeover": shape([]string{"type"}),
	"ping":     shape([]string{"type", "nonce"}),
	"pong":     shape([]string{"type", "nonce"}),
}

func protocolError(code, message, requestID string, retryable bool) Frame {
	return Frame{
		Type:      "error",
		Code:      code,
		Message:   message,
		RequestID: requestID,
		Retryable: retryable,
	}
}
