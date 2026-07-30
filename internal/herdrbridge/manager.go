package herdrbridge

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
)

type Manager struct {
	config Config
	api    *Client
	store  *StateStore
	herdr  *HerdrClient
	in     *bufio.Reader
	out    io.Writer
}

func NewManager(configuration Config, api *Client, store *StateStore, herdr *HerdrClient, input io.Reader, output io.Writer) *Manager {
	return &Manager{config: configuration, api: api, store: store, herdr: herdr, in: bufio.NewReader(input), out: output}
}

func (m *Manager) Run(ctx context.Context, action string) error {
	if action == "" || action == "manage" {
		selected, err := m.chooseAction()
		if err != nil {
			return err
		}
		action = selected
	}
	if action == "create" {
		return m.create(ctx)
	}
	if action != "attach" && action != "takeover" && action != "stop" && action != "resume" && action != "focus" && action != "delete" {
		return fmt.Errorf("unsupported manager action %q", action)
	}
	agent, err := m.chooseAgent(ctx, action)
	if err != nil {
		return err
	}
	state := m.stateFor(agent)
	switch action {
	case "attach", "takeover":
		if agent.Status.State == lifecycle.StateStopped {
			fmt.Fprintln(m.out, "Resuming agent before attachment…")
			if _, err := m.api.Resume(ctx, agent.ID); err != nil {
				return err
			}
		}
		if err := m.store.Save(state); err != nil {
			return err
		}
		if action == "attach" && state.PaneID != "" && m.herdr.FocusPane(ctx, state.PaneID) == nil {
			fmt.Fprintln(m.out, "Focused the existing agent pane.")
			return nil
		}
		fmt.Fprintln(m.out, "Opening agent pane…")
		return m.herdr.OpenAgent(ctx, agent.ID, managerTargetPane(), action == "takeover")
	case "focus":
		if err := m.herdr.FocusPane(ctx, state.PaneID); err == nil {
			return nil
		}
		fmt.Fprintln(m.out, "The remembered pane is gone; opening a replacement…")
		return m.herdr.OpenAgent(ctx, agent.ID, managerTargetPane(), false)
	case "stop":
		updated, err := m.api.Stop(ctx, agent.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(m.out, "%s is %s. The remote agent remains available to resume.\n", updated.Name, updated.Status.State)
		return nil
	case "resume":
		updated, err := m.api.Resume(ctx, agent.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(m.out, "%s is %s.\n", updated.Name, updated.Status.State)
		return nil
	case "delete":
		fmt.Fprintf(m.out, "Delete %s permanently? Type its name to confirm: ", agent.Name)
		confirmation, err := m.readLine()
		if err != nil {
			return err
		}
		if confirmation != agent.Name {
			return fmt.Errorf("delete cancelled")
		}
		if _, err := m.api.Delete(ctx, agent.ID); err != nil {
			return err
		}
		_ = m.herdr.ClosePane(ctx, state.PaneID)
		_ = m.store.Remove(agent.ID)
		fmt.Fprintf(m.out, "Deletion requested for %s.\n", agent.Name)
		return nil
	}
	return nil
}

func (m *Manager) create(ctx context.Context) error {
	fmt.Fprint(m.out, "New agent name (lowercase letters, numbers, hyphens): ")
	name, err := m.readLine()
	if err != nil {
		return err
	}
	if !lifecycle.ValidName(name) {
		return fmt.Errorf("agent name must be a DNS label between 1 and 63 characters")
	}
	agent, err := m.api.Create(ctx, m.config.CreateRequest(name))
	if err != nil {
		return err
	}
	state := m.stateFor(agent)
	if err := m.store.Save(state); err != nil {
		return err
	}
	fmt.Fprintf(m.out, "Created %s (%s). Opening its pane…\n", agent.Name, agent.ID)
	return m.herdr.OpenAgent(ctx, agent.ID, managerTargetPane(), false)
}

func (m *Manager) chooseAction() (string, error) {
	fmt.Fprintln(m.out, "Sandherd")
	fmt.Fprintln(m.out, "  1. Create agent")
	fmt.Fprintln(m.out, "  2. Attach agent")
	fmt.Fprintln(m.out, "  3. Stop agent")
	fmt.Fprintln(m.out, "  4. Resume agent")
	fmt.Fprintln(m.out, "  5. Focus agent pane")
	fmt.Fprintln(m.out, "  6. Take over agent control")
	fmt.Fprintln(m.out, "  7. Delete agent")
	fmt.Fprint(m.out, "Choose: ")
	selection, err := m.readLine()
	if err != nil {
		return "", err
	}
	actions := map[string]string{"1": "create", "2": "attach", "3": "stop", "4": "resume", "5": "focus", "6": "takeover", "7": "delete"}
	action := actions[selection]
	if action == "" {
		return "", fmt.Errorf("invalid action selection")
	}
	return action, nil
}

func (m *Manager) chooseAgent(ctx context.Context, action string) (lifecycle.Agent, error) {
	list, err := m.api.List(ctx)
	if err != nil {
		return lifecycle.Agent{}, err
	}
	agents := make([]lifecycle.Agent, 0, len(list.Items))
	for _, agent := range list.Items {
		if applicableAction(action, agent.Status.State) {
			agents = append(agents, agent)
		}
	}
	if len(agents) == 0 {
		return lifecycle.Agent{}, fmt.Errorf("no agents can %s right now", action)
	}
	fmt.Fprintf(m.out, "Agents available to %s:\n", action)
	for index, agent := range agents {
		fmt.Fprintf(m.out, "  %d. %-24s %s\n", index+1, agent.Name, agent.Status.State)
	}
	fmt.Fprint(m.out, "Choose: ")
	selection, err := m.readLine()
	if err != nil {
		return lifecycle.Agent{}, err
	}
	index, err := strconv.Atoi(selection)
	if err != nil || index < 1 || index > len(agents) {
		return lifecycle.Agent{}, fmt.Errorf("invalid agent selection")
	}
	return agents[index-1], nil
}

func (m *Manager) stateFor(agent lifecycle.Agent) AgentState {
	state, err := m.store.Load(agent.ID)
	if err != nil {
		state = AgentState{AgentID: agent.ID, Name: agent.Name, BaseURL: m.api.BaseURL()}
	}
	state.Name = agent.Name
	state.BaseURL = m.api.BaseURL()
	return state
}

func (m *Manager) readLine() (string, error) {
	line, err := m.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func applicableAction(action string, state lifecycle.State) bool {
	switch action {
	case "stop":
		return lifecycle.CanStop(state) && state != lifecycle.StateStopped
	case "resume":
		return lifecycle.CanResume(state) && state == lifecycle.StateStopped
	case "delete":
		return state != lifecycle.StateDeleting
	default:
		return state != lifecycle.StateDeleting && state != lifecycle.StateFailed
	}
}

func managerTargetPane() string {
	if value := os.Getenv("SANDHERD_TARGET_PANE_ID"); value != "" {
		return value
	}
	return actionTargetPane()
}
