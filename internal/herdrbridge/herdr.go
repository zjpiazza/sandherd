package herdrbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
)

const pluginID = "dev.sandherd"

type CommandRunner interface {
	Run(context.Context, []string, map[string]string) ([]byte, error)
}

type ExecCommandRunner struct{ Binary string }

func (r ExecCommandRunner) Run(ctx context.Context, arguments []string, extraEnvironment map[string]string) ([]byte, error) {
	binary := r.Binary
	if binary == "" {
		binary = os.Getenv("HERDR_BIN_PATH")
	}
	if binary == "" {
		binary = "herdr"
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = os.Environ()
	for name, value := range extraEnvironment {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("herdr %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type HerdrClient struct {
	runner CommandRunner
	seq    atomic.Uint64
}

func NewHerdrClient(runner CommandRunner) *HerdrClient {
	if runner == nil {
		runner = ExecCommandRunner{}
	}
	return &HerdrClient{runner: runner}
}

func (h *HerdrClient) ReportAgent(ctx context.Context, paneID, state, message string) error {
	if paneID == "" {
		return nil
	}
	arguments := []string{"pane", "report-agent", paneID, "--source", "plugin:" + currentPluginID(), "--agent", "sandherd", "--state", state, "--seq", strconv.FormatUint(h.seq.Add(1), 10)}
	if message != "" {
		arguments = append(arguments, "--message", message)
	}
	_, err := h.runner.Run(ctx, arguments, nil)
	return err
}

func (h *HerdrClient) ReportMetadata(ctx context.Context, paneID string, state AgentState) error {
	if paneID == "" {
		return nil
	}
	arguments := []string{
		"pane", "report-metadata", paneID,
		"--source", "plugin:" + currentPluginID(),
		"--agent", "sandherd",
		"--title", state.Name,
		"--display-agent", "Sandherd: " + state.Name,
		"--token", "sandherd_agent_id=" + state.AgentID,
		"--seq", strconv.FormatUint(h.seq.Add(1), 10),
	}
	_, err := h.runner.Run(ctx, arguments, nil)
	return err
}

func (h *HerdrClient) Notify(ctx context.Context, title, body, sound string) error {
	arguments := []string{"notification", "show", title}
	if body != "" {
		arguments = append(arguments, "--body", body)
	}
	if sound != "" {
		arguments = append(arguments, "--sound", sound)
	}
	_, err := h.runner.Run(ctx, arguments, nil)
	return err
}

func (h *HerdrClient) OpenManager(ctx context.Context, action, targetPaneID string) error {
	arguments := []string{"plugin", "pane", "open", "--plugin", currentPluginID(), "--entrypoint", "manager", "--placement", "popup", "--width", "80%", "--height", "70%", "--focus", "--env", "SANDHERD_ACTION=" + action}
	if targetPaneID != "" {
		arguments = append(arguments, "--env", "SANDHERD_TARGET_PANE_ID="+targetPaneID)
	}
	_, err := h.runner.Run(ctx, arguments, nil)
	return err
}

func (h *HerdrClient) OpenAgent(ctx context.Context, agentID, targetPaneID string, takeover bool) error {
	placement := agentPanePlacement()
	arguments := []string{"plugin", "pane", "open", "--plugin", currentPluginID(), "--entrypoint", "agent", "--placement", placement, "--focus", "--env", "SANDHERD_AGENT_ID=" + agentID}
	if placement == "split" {
		arguments = append(arguments, "--direction", "right")
	}
	if placement == "split" && targetPaneID != "" {
		arguments = append(arguments, "--target-pane", targetPaneID)
	}
	if placement == "tab" && os.Getenv("HERDR_WORKSPACE_ID") != "" {
		arguments = append(arguments, "--workspace", os.Getenv("HERDR_WORKSPACE_ID"))
	}
	if takeover {
		arguments = append(arguments, "--env", "SANDHERD_TAKEOVER=1")
	}
	_, err := h.runner.Run(ctx, arguments, nil)
	return err
}

func agentPanePlacement() string {
	if configured := os.Getenv("SANDHERD_AGENT_PLACEMENT"); configured == "split" || configured == "tab" {
		return configured
	}
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 && columns < 100 {
		return "tab"
	}
	return "split"
}

func (h *HerdrClient) FocusPane(ctx context.Context, paneID string) error {
	if paneID == "" {
		return fmt.Errorf("agent has no remembered Herdr pane")
	}
	_, err := h.runner.Run(ctx, []string{"plugin", "pane", "focus", paneID}, nil)
	return err
}

func (h *HerdrClient) ClosePane(ctx context.Context, paneID string) error {
	if paneID == "" {
		return nil
	}
	_, err := h.runner.Run(ctx, []string{"plugin", "pane", "close", paneID}, nil)
	return err
}

func currentPluginID() string {
	if value := os.Getenv("HERDR_PLUGIN_ID"); value != "" {
		return value
	}
	return pluginID
}

func CurrentPaneID() string { return os.Getenv("HERDR_PANE_ID") }

func actionTargetPane() string {
	if paneID := os.Getenv("HERDR_PANE_ID"); paneID != "" {
		return paneID
	}
	contextJSON := os.Getenv("HERDR_PLUGIN_CONTEXT_JSON")
	var contextValue struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
		FocusedPane struct {
			PaneID string `json:"pane_id"`
		} `json:"focused_pane"`
	}
	if json.Unmarshal([]byte(contextJSON), &contextValue) == nil {
		if contextValue.Pane.PaneID != "" {
			return contextValue.Pane.PaneID
		}
		return contextValue.FocusedPane.PaneID
	}
	return ""
}
