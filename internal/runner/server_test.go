package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	testControlToken = "control-token-for-tests"
	testObserveToken = "observe-token-for-tests"
)

func newTestServer(t *testing.T) (*hub, *httptest.Server) {
	t.Helper()
	h, _ := testHub(t, 1024, 16, time.Minute)
	api := &server{
		hub: h,
		authenticator: staticAuthenticator{
			controlToken: []byte(testControlToken),
			observeToken: []byte(testObserveToken),
		},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		attachTimeout: time.Second,
		writeTimeout:  time.Second,
	}
	httpServer := httptest.NewServer(api.handler())
	t.Cleanup(httpServer.Close)
	return h, httpServer
}

func dialTerminal(t *testing.T, httpServer *httptest.Server, token string, attach Frame) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1alpha1/terminal"
	connection, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{Protocol},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("dial terminal: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatalf("dial terminal: %v", err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	if err := wsjson.Write(ctx, connection, attach); err != nil {
		t.Fatalf("write attach: %v", err)
	}
	return connection
}

func readTerminalFrame(t *testing.T, connection *websocket.Conn) Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var frame Frame
	if err := wsjson.Read(ctx, connection, &frame); err != nil {
		t.Fatalf("read terminal frame: %v", err)
	}
	return frame
}

func terminalAttach(role string, after *uint64) Frame {
	return Frame{
		Type:            "attach",
		ProtocolVersion: ProtocolVersion,
		Role:            role,
		AfterSequence:   after,
		TerminalSize:    &TerminalSize{Columns: 80, Rows: 24},
	}
}

func TestTerminalReconnectReplaysThenStreamsLive(t *testing.T) {
	h, httpServer := newTestServer(t)
	h.publishOutput([]byte("before-attach"))
	first := dialTerminal(t, httpServer, testObserveToken, terminalAttach("observe", nil))
	attached := readTerminalFrame(t, first)
	if attached.Type != "attached" || attached.Role != "observe" || *attached.LatestSequence != 1 {
		t.Fatalf("attached frame = %#v", attached)
	}
	h.publishOutput([]byte{0x00, 0xff, 'a'})
	live := readTerminalFrame(t, first)
	if live.Type != "output" || *live.Sequence != 2 || string(live.Data) != string([]byte{0x00, 0xff, 'a'}) {
		t.Fatalf("live frame = %#v", live)
	}
	_ = first.Close(websocket.StatusNormalClosure, "reconnect")

	h.publishOutput([]byte("while-away"))
	cursor := uint64(2)
	second := dialTerminal(t, httpServer, testObserveToken, terminalAttach("observe", &cursor))
	if frame := readTerminalFrame(t, second); frame.Type != "attached" {
		t.Fatalf("first reconnect frame = %#v, want attached", frame)
	}
	replayed := readTerminalFrame(t, second)
	if replayed.Type != "output" || *replayed.Sequence != 3 || string(replayed.Data) != "while-away" {
		t.Fatalf("replayed frame = %#v", replayed)
	}
	h.publishOutput([]byte("live-again"))
	next := readTerminalFrame(t, second)
	if next.Type != "output" || *next.Sequence != 4 || string(next.Data) != "live-again" {
		t.Fatalf("next frame = %#v", next)
	}
}

func TestObserveCredentialCannotControlTakeOverSignalOrStop(t *testing.T) {
	_, httpServer := newTestServer(t)
	connection := dialTerminal(t, httpServer, testObserveToken, terminalAttach("observe", nil))
	if frame := readTerminalFrame(t, connection); frame.Type != "attached" {
		t.Fatalf("attach response = %#v", frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, connection, Frame{Type: "takeover"}); err != nil {
		t.Fatalf("write takeover: %v", err)
	}
	if frame := readTerminalFrame(t, connection); frame.Type != "error" || frame.Code != "forbidden_role" {
		t.Fatalf("takeover response = %#v", frame)
	}
	if err := wsjson.Write(ctx, connection, Frame{Type: "input", LeaseID: newID(), Data: []byte("no")}); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if frame := readTerminalFrame(t, connection); frame.Type != "error" || frame.Code != "invalid_controller_lease" {
		t.Fatalf("input response = %#v", frame)
	}

	for _, endpoint := range []string{"signal", "stop"} {
		request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v1alpha1/%s", httpServer.URL, endpoint), strings.NewReader(`{"signal":"SIGINT"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+testObserveToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", endpoint, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s status = %d, want 403", endpoint, response.StatusCode)
		}
	}
}

func TestRunnerEndpointsRequireAuthentication(t *testing.T) {
	_, httpServer := newTestServer(t)
	for _, path := range []string{"/v1alpha1/metadata", "/v1alpha1/terminal"} {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s status = %d, want 401", path, response.StatusCode)
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, response.StatusCode)
		}
	}
}

func TestTerminalRejectsUnknownFrameAfterFlushingError(t *testing.T) {
	_, httpServer := newTestServer(t)
	connection := dialTerminal(t, httpServer, testObserveToken, terminalAttach("observe", nil))
	if frame := readTerminalFrame(t, connection); frame.Type != "attached" {
		t.Fatalf("attach response = %#v", frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, connection, map[string]any{"type": "output", "sequence": 1, "data": "eA=="}); err != nil {
		t.Fatalf("write invalid client frame: %v", err)
	}
	frame := readTerminalFrame(t, connection)
	if frame.Type != "error" || frame.Code != "unsupported_protocol" {
		t.Fatalf("invalid frame response = %#v", frame)
	}
	var ignored Frame
	if err := wsjson.Read(ctx, connection, &ignored); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v (%v), want policy violation", websocket.CloseStatus(err), err)
	}
}
