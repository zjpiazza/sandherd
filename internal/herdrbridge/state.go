package herdrbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type AgentState struct {
	AgentID          string    `json:"agentId"`
	Name             string    `json:"name"`
	BaseURL          string    `json:"baseUrl"`
	PaneID           string    `json:"paneId,omitempty"`
	AfterSequence    *uint64   `json:"afterSequence,omitempty"`
	RunnerGeneration string    `json:"runnerGeneration,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type StateStore struct{ directory string }

func NewStateStore(directory string) (*StateStore, error) {
	if directory == "" {
		return nil, fmt.Errorf("state directory is required")
	}
	directory = filepath.Join(filepath.Clean(directory), "agents")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	return &StateStore{directory: directory}, nil
}

func (s *StateStore) Load(agentID string) (AgentState, error) {
	if !validAgentID(agentID) {
		return AgentState{}, fmt.Errorf("invalid agent ID")
	}
	contents, err := os.ReadFile(filepath.Join(s.directory, agentID+".json"))
	if err != nil {
		return AgentState{}, err
	}
	var state AgentState
	if err := json.Unmarshal(contents, &state); err != nil {
		return AgentState{}, fmt.Errorf("decode agent state: %w", err)
	}
	if state.AgentID != agentID {
		return AgentState{}, fmt.Errorf("agent state identity mismatch")
	}
	return state, nil
}

func (s *StateStore) Save(state AgentState) error {
	if !validAgentID(state.AgentID) || state.Name == "" || state.BaseURL == "" {
		return fmt.Errorf("agent ID, name, and base URL are required")
	}
	state.UpdatedAt = time.Now().UTC()
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.directory, ".agent-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(s.directory, state.AgentID+".json"))
}

func (s *StateStore) Remove(agentID string) error {
	if !validAgentID(agentID) {
		return fmt.Errorf("invalid agent ID")
	}
	err := os.Remove(filepath.Join(s.directory, agentID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *StateStore) List() ([]AgentState, error) {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, err
	}
	states := make([]AgentState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		agentID := strings.TrimSuffix(entry.Name(), ".json")
		state, loadErr := s.Load(agentID)
		if loadErr == nil {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states, nil
}

func validAgentID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}
