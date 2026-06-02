package acp

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RuntimeSessionMode describes how the session should be persisted.
type RuntimeSessionMode string

const (
	RuntimeSessionModePersistent RuntimeSessionMode = "persistent"
	RuntimeSessionModeOneshot    RuntimeSessionMode = "oneshot"
)

// RuntimeEnsureInput describes a session bootstrap request.
type RuntimeEnsureInput struct {
	Agent           string
	CWD             string
	Name            string
	SessionKey      string
	Mode            RuntimeSessionMode
	ResumeSessionID string
	SessionOptions  *SessionOptions
}

// RuntimeHandle is the durable session reference returned to callers.
type RuntimeHandle struct {
	RecordID         string `json:"recordId"`
	SessionKey       string `json:"sessionKey,omitempty"`
	Agent            string `json:"agent"`
	CWD              string `json:"cwd"`
	Name             string `json:"name,omitempty"`
	BackendSessionID string `json:"backendSessionId,omitempty"`
}

// RuntimeEventType is the normalized event type emitted by the runtime.
type RuntimeEventType string

const (
	RuntimeEventTextDelta            RuntimeEventType = "text_delta"
	RuntimeEventToolCall             RuntimeEventType = "tool_call"
	RuntimeEventToolResult           RuntimeEventType = "tool_result"
	RuntimeEventPermissionEscalation RuntimeEventType = "permission_escalation"
	RuntimeEventStatus               RuntimeEventType = "status"
)

// RuntimeEvent is emitted while a turn is running.
type RuntimeEvent struct {
	Type                 RuntimeEventType           `json:"type"`
	Text                 string                     `json:"text,omitempty"`
	ToolName             string                     `json:"toolName,omitempty"`
	ToolID               string                     `json:"toolId,omitempty"`
	ToolTitle            string                     `json:"toolTitle,omitempty"`
	ToolArgs             string                     `json:"toolArgs,omitempty"`
	IsError              bool                       `json:"isError,omitempty"`
	Status               string                     `json:"status,omitempty"`
	PermissionEscalation *PermissionEscalationEvent `json:"permissionEscalation,omitempty"`
}

// RuntimeTurnInput describes a prompt execution request.
type RuntimeTurnInput struct {
	Handle RuntimeHandle
	Text   string
}

type SystemPromptOption struct {
	Append string `json:"append,omitempty"`
}

type SessionOptions struct {
	Model        string      `json:"model,omitempty"`
	AllowedTools []string    `json:"allowedTools,omitempty"`
	MaxTurns     int         `json:"maxTurns,omitempty"`
	SystemPrompt interface{} `json:"systemPrompt,omitempty"` // string or SystemPromptOption
}

func cloneSessionOptions(options *SessionOptions) *SessionOptions {
	if options == nil {
		return nil
	}
	cloned := *options
	if len(options.AllowedTools) > 0 {
		cloned.AllowedTools = append([]string(nil), options.AllowedTools...)
	}
	switch value := options.SystemPrompt.(type) {
	case string:
		cloned.SystemPrompt = value
	case SystemPromptOption:
		cloned.SystemPrompt = value
	case *SystemPromptOption:
		if value != nil {
			copyValue := *value
			cloned.SystemPrompt = copyValue
		} else {
			cloned.SystemPrompt = nil
		}
	case map[string]interface{}:
		copyMap := make(map[string]interface{}, len(value))
		for key, v := range value {
			copyMap[key] = v
		}
		cloned.SystemPrompt = copyMap
	}
	return &cloned
}

// RuntimeTurnResult is the durable result of a completed turn.
type RuntimeTurnResult struct {
	Text       string         `json:"text"`
	StopReason StopReason     `json:"stopReason"`
	Record     *SessionRecord `json:"record"`
}

// RuntimeStatus is the session status view exposed by the runtime.
type RuntimeStatus struct {
	RecordID         string            `json:"recordId"`
	SessionKey       string            `json:"sessionKey,omitempty"`
	Agent            string            `json:"agent"`
	CWD              string            `json:"cwd"`
	Name             string            `json:"name,omitempty"`
	Title            string            `json:"title,omitempty"`
	BackendSessionID string            `json:"backendSessionId,omitempty"`
	Status           string            `json:"status,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	AvailableModes   []string          `json:"availableModes,omitempty"`
	AvailableModels  []string          `json:"availableModels,omitempty"`
	Closed           bool              `json:"closed"`
	Summary          string            `json:"summary,omitempty"`
	LastStopReason   string            `json:"lastStopReason,omitempty"`
	LastPrompt       string            `json:"lastPrompt,omitempty"`
	LastPromptAt     *time.Time        `json:"lastPromptAt,omitempty"`
	LastError        string            `json:"lastError,omitempty"`
	ActiveRequestID  string            `json:"activeRequestId,omitempty"`
	OwnerPID         int               `json:"ownerPid,omitempty"`
	OwnerStartedAt   *time.Time        `json:"ownerStartedAt,omitempty"`
	Uptime           string            `json:"uptime,omitempty"`
	ConfigValues     map[string]string `json:"configValues,omitempty"`
	QueueState       string            `json:"queueState,omitempty"`
	QueueDepth       int               `json:"queueDepth,omitempty"`
	OwnerAlive       bool              `json:"ownerAlive,omitempty"`
	OwnerHealthy     bool              `json:"ownerHealthy,omitempty"`
}

// RuntimeManagerOptions configures the higher-level runtime facade.
type RuntimeManagerOptions struct {
	StateDir   string
	WorkingDir string
	Registry   AgentRegistry
	Store      SessionStore
	History    HistoryStore
	Policy     PermissionPolicy
	MCPServers []MCPServer
}

func DefaultStateDir() string {
	if value := strings.TrimSpace(os.Getenv("GGCODE_ACP_STATE_DIR")); value != "" {
		return filepath.Clean(value)
	}
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(filepath.Clean(value), "ggcode-acp-go")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ".ggcode-acp-go"
	}
	return filepath.Join(home, ".ggcode-acp-go")
}
