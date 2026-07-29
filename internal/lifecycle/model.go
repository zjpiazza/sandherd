// Package lifecycle defines Sandherd's transport-neutral agent model and state machine.
package lifecycle

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

type State string

const (
	StateRequested    State = "requested"
	StateProvisioning State = "provisioning"
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateStopping     State = "stopping"
	StateStopped      State = "stopped"
	StateFailed       State = "failed"
	StateDeleting     State = "deleting"
)

type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
	DesiredDeleted DesiredState = "deleted"
)

type RepositorySpec struct {
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
}

type ResourceSpec struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type WorkspaceSpec struct {
	Size            string `json:"size"`
	StorageProfile  string `json:"storageProfile,omitempty"`
	RetentionPolicy string `json:"retentionPolicy"`
}

type LifecycleSpec struct {
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds"`
}

type AgentSpec struct {
	Kind           string          `json:"kind"`
	SandboxProfile string          `json:"sandboxProfile"`
	Repository     *RepositorySpec `json:"repository,omitempty"`
	Resources      ResourceSpec    `json:"resources"`
	Workspace      WorkspaceSpec   `json:"workspace"`
	SecretProfile  string          `json:"secretProfile,omitempty"`
	Lifecycle      LifecycleSpec   `json:"lifecycle"`
}

type AgentStatus struct {
	State              State      `json:"state"`
	ObservedGeneration int64      `json:"observedGeneration"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
	ReadyAt            *time.Time `json:"readyAt,omitempty"`
	StoppedAt          *time.Time `json:"stoppedAt,omitempty"`
	LastTransitionAt   *time.Time `json:"lastTransitionAt,omitempty"`
}

type Agent struct {
	APIVersion      string       `json:"apiVersion"`
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Owner           string       `json:"owner"`
	Generation      int64        `json:"generation"`
	ResourceVersion string       `json:"-"`
	Spec            AgentSpec    `json:"spec"`
	Status          AgentStatus  `json:"status"`
	DesiredState    DesiredState `json:"-"`
	CreatedAt       time.Time    `json:"createdAt"`
	UpdatedAt       time.Time    `json:"updatedAt"`
}

type CreateRequest struct {
	Name string    `json:"name"`
	Spec AgentSpec `json:"spec"`
}

type AgentList struct {
	Items      []Agent `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

type Event struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	AgentID       string    `json:"agentId"`
	PreviousState State     `json:"previousState,omitempty"`
	State         State     `json:"state,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	RequestID     string    `json:"requestId,omitempty"`
	Owner         string    `json:"-"`
}

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	profilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

func (r CreateRequest) Validate() error {
	if !ValidName(r.Name) {
		return fmt.Errorf("name must be a DNS label between 1 and 63 characters")
	}
	if len(r.Spec.Kind) < 1 || len(r.Spec.Kind) > 64 || !profilePattern.MatchString(r.Spec.Kind) {
		return fmt.Errorf("spec.kind is invalid")
	}
	if len(r.Spec.SandboxProfile) < 1 || len(r.Spec.SandboxProfile) > 64 || !profilePattern.MatchString(r.Spec.SandboxProfile) {
		return fmt.Errorf("spec.sandboxProfile is invalid")
	}
	if r.Spec.Resources.CPU == "" || r.Spec.Resources.Memory == "" {
		return fmt.Errorf("spec.resources.cpu and memory are required")
	}
	if r.Spec.Workspace.Size == "" {
		return fmt.Errorf("spec.workspace.size is required")
	}
	if r.Spec.Workspace.RetentionPolicy != "delete" && r.Spec.Workspace.RetentionPolicy != "retain" {
		return fmt.Errorf("spec.workspace.retentionPolicy must be delete or retain")
	}
	if r.Spec.Repository != nil {
		parsed, err := url.Parse(r.Spec.Repository.URL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" {
			return fmt.Errorf("spec.repository.url must be an HTTPS or SSH URL")
		}
		if len(r.Spec.Repository.Revision) > 256 {
			return fmt.Errorf("spec.repository.revision must not exceed 256 characters")
		}
	}
	if r.Spec.Workspace.StorageProfile != "" && (len(r.Spec.Workspace.StorageProfile) > 64 || !profilePattern.MatchString(r.Spec.Workspace.StorageProfile)) {
		return fmt.Errorf("spec.workspace.storageProfile is invalid")
	}
	if r.Spec.SecretProfile != "" && (len(r.Spec.SecretProfile) > 64 || !profilePattern.MatchString(r.Spec.SecretProfile)) {
		return fmt.Errorf("spec.secretProfile is invalid")
	}
	if r.Spec.Lifecycle.IdleTimeoutSeconds < 0 || r.Spec.Lifecycle.IdleTimeoutSeconds > 604800 {
		return fmt.Errorf("spec.lifecycle.idleTimeoutSeconds must be between 0 and 604800")
	}
	return nil
}

func (r *CreateRequest) ApplyDefaults() {
	if r.Spec.Repository != nil && r.Spec.Repository.Revision == "" {
		r.Spec.Repository.Revision = "HEAD"
	}
	if r.Spec.Workspace.StorageProfile == "" {
		r.Spec.Workspace.StorageProfile = "default"
	}
}

func ValidName(name string) bool {
	return len(name) >= 1 && len(name) <= 63 && namePattern.MatchString(name)
}

func CanStop(state State) bool {
	return state == StateRunning || state == StateStopping || state == StateStopped
}

func CanResume(state State) bool {
	return state == StateStopped || state == StateProvisioning || state == StateStarting || state == StateRunning
}

// NewID returns a time-ordered UUIDv7 without an external identity dependency.
func NewID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	milliseconds := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint16(value[0:2], uint16(milliseconds>>32))
	binary.BigEndian.PutUint32(value[2:6], uint32(milliseconds))
	value[6] = (value[6] & 0x0f) | 0x70
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
