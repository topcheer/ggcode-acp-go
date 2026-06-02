package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RuntimeManager provides a higher-level acpx-style runtime facade on top of the ACP client.
type RuntimeManager struct {
	stateDir   string
	workingDir string
	registry   AgentRegistry
	store      SessionStore
	history    HistoryStore
	policy     PermissionPolicy
	mcpServers []MCPServer
}

type ImportSessionOptions struct {
	Name string
	CWD  string
}

type PruneClosedSessionsOptions struct {
	OlderThan      time.Duration
	Before         *time.Time
	IncludeHistory bool
	DryRun         bool
}

type PruneClosedSessionsResult struct {
	Records    []*SessionRecord `json:"records"`
	Deleted    []string         `json:"deleted"`
	DryRun     bool             `json:"dryRun"`
	BytesFreed int64            `json:"bytesFreed"`
}

type RuntimeTurnCallbacks struct {
	OnEvent                  func(RuntimeEvent)
	OnRawMessage             func(json.RawMessage)
	OnActiveController       func(RuntimeActiveSessionController)
	OnActiveControllerClosed func()
}

type RuntimeActiveSessionController interface {
	SetSessionMode(context.Context, SessionModeId) error
	SetSessionModel(context.Context, string) error
	SetSessionConfigOption(context.Context, SessionConfigId, SessionConfigValueId) (*SetSessionConfigOptionResponse, error)
	CloseSession(context.Context) error
}

type runtimeActiveSessionController struct {
	client *Client
}

func (c runtimeActiveSessionController) SetSessionMode(ctx context.Context, mode SessionModeId) error {
	return c.client.SetSessionMode(ctx, mode)
}

func (c runtimeActiveSessionController) SetSessionModel(ctx context.Context, modelID string) error {
	return c.client.SetSessionModel(ctx, modelID)
}

func (c runtimeActiveSessionController) SetSessionConfigOption(ctx context.Context, configID SessionConfigId, value SessionConfigValueId) (*SetSessionConfigOptionResponse, error) {
	return c.client.SetSessionConfigOption(ctx, configID, value)
}

func (c runtimeActiveSessionController) CloseSession(ctx context.Context) error {
	c.client.mu.Lock()
	sessionID := c.client.sessionID
	c.client.mu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("session is not initialized")
	}
	c.client.closeSession(sessionID)
	return nil
}

// SessionExport is the portable archive used by export/import.
type SessionExport struct {
	Record  SessionRecord         `json:"record"`
	History []SessionHistoryEntry `json:"history"`
}

func NewRuntimeManager(opts RuntimeManagerOptions) *RuntimeManager {
	stateDir := strings.TrimSpace(opts.StateDir)
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	workingDir := strings.TrimSpace(opts.WorkingDir)
	if workingDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			workingDir = cwd
		}
	}
	registry := opts.Registry
	if registry == nil {
		registry = NewStaticAgentRegistry(nil)
	}
	store := opts.Store
	if store == nil {
		store = NewFileSessionStore(stateDir)
	}
	history := opts.History
	if history == nil {
		history = NewFileHistoryStore(stateDir)
	}
	return &RuntimeManager{
		stateDir:   filepath.Clean(stateDir),
		workingDir: workingDir,
		registry:   registry,
		store:      store,
		history:    history,
		policy:     opts.Policy,
		mcpServers: cloneMCPServers(opts.MCPServers),
	}
}

func (m *RuntimeManager) resolveCWD(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		cwd = m.workingDir
	}
	if cwd == "" {
		cwd = "."
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		return abs
	}
	return filepath.Clean(cwd)
}

func normalizeSessionName(name, cwd string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	base := filepath.Base(filepath.Clean(cwd))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "default"
	}
	return base
}

func scopedSessionKey(agent, cwd, name string) string {
	return NormalizeAgentName(agent) + "|" + filepath.Clean(cwd) + "|" + normalizeSessionName(name, cwd)
}

func recordToHandle(record *SessionRecord) *RuntimeHandle {
	if record == nil {
		return nil
	}
	return &RuntimeHandle{
		RecordID:         record.RecordID,
		SessionKey:       record.SessionKey,
		Agent:            record.Agent,
		CWD:              record.CWD,
		Name:             record.Name,
		BackendSessionID: record.BackendSessionID,
	}
}

func cloneConfigValues(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func formatRuntimeUptime(startedAt *time.Time) string {
	if startedAt == nil || startedAt.IsZero() {
		return ""
	}
	elapsed := time.Since(*startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int(elapsed / time.Second)
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, secs)
}

func summarizeRuntimeStatus(status string) string {
	switch status {
	case "running":
		return "queue owner healthy"
	case "dead":
		return "queue owner unavailable"
	default:
		return "session idle; queue owner will start on next prompt"
	}
}

func extractAvailableModes(state *SessionModeState) []string {
	if state == nil || len(state.Modes) == 0 {
		return nil
	}
	modes := make([]string, 0, len(state.Modes))
	for _, mode := range state.Modes {
		id := strings.TrimSpace(string(mode.ID))
		if id == "" {
			continue
		}
		modes = append(modes, id)
	}
	return modes
}

func extractAvailableModels(options []SessionConfigOption) []string {
	for _, option := range options {
		if strings.TrimSpace(string(option.ID)) != "model" && strings.TrimSpace(option.Category) != "model" {
			continue
		}
		models := make([]string, 0)
		for _, value := range flattenConfigSelectOptions(option.Options) {
			id := strings.TrimSpace(string(value.ID))
			if id == "" {
				continue
			}
			models = append(models, id)
		}
		return models
	}
	return nil
}

func applyClientSessionState(record *SessionRecord, client *Client) {
	if record == nil || client == nil {
		return
	}
	title, modes, configOptions, commands := client.CurrentSessionState()
	if strings.TrimSpace(title) != "" {
		record.Title = title
	}
	if modes != nil {
		record.Mode = strings.TrimSpace(string(modes.Current))
		record.AvailableModes = extractAvailableModes(modes)
	}
	record.ConfigOptions = cloneSessionConfigOptions(configOptions)
	record.AvailableCommands = cloneAvailableCommands(commands)
	if len(configOptions) > 0 {
		record.AvailableModels = extractAvailableModels(configOptions)
		if record.ConfigValues == nil {
			record.ConfigValues = map[string]string{}
		}
		for _, option := range configOptions {
			if selected := strings.TrimSpace(string(option.CurrentValue)); selected != "" {
				record.ConfigValues[string(option.ID)] = selected
			}
		}
	}
}

func flattenConfigSelectOptions(options SessionConfigSelectOptions) []SessionConfigSelectOption {
	if len(options) == 0 {
		return nil
	}
	flattened := make([]SessionConfigSelectOption, 0, len(options))
	for _, raw := range options {
		switch value := raw.(type) {
		case SessionConfigSelectOption:
			flattened = append(flattened, value)
		case *SessionConfigSelectOption:
			if value != nil {
				flattened = append(flattened, *value)
			}
		case SessionConfigSelectGroup:
			flattened = append(flattened, value.Options...)
		case *SessionConfigSelectGroup:
			if value != nil {
				flattened = append(flattened, value.Options...)
			}
		case map[string]interface{}:
			if groupOptions, ok := value["options"].([]interface{}); ok {
				for _, grouped := range flattenConfigSelectOptions(SessionConfigSelectOptions(groupOptions)) {
					flattened = append(flattened, grouped)
				}
				continue
			}
			id, _ := value["id"].(string)
			name, _ := value["name"].(string)
			groupID, _ := value["groupId"].(string)
			if strings.TrimSpace(id) != "" {
				flattened = append(flattened, SessionConfigSelectOption{ID: id, Name: name, GroupID: groupID})
			}
		}
	}
	return flattened
}

func (m *RuntimeManager) LoadRecord(recordID string) (*SessionRecord, error) {
	return m.store.Load(recordID)
}

func (m *RuntimeManager) FindSession(agent, cwd, name string, walkParents bool) (*SessionRecord, error) {
	records, err := m.ListSessions(agent, "", true)
	if err != nil {
		return nil, err
	}
	cwd = m.resolveCWD(cwd)
	name = normalizeSessionName(name, cwd)
	current := filepath.Clean(cwd)
	for {
		for _, record := range records {
			if record.Agent != NormalizeAgentName(agent) {
				continue
			}
			if record.Name != name {
				continue
			}
			if filepath.Clean(record.CWD) == current {
				return record, nil
			}
		}
		if !walkParents {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil, nil
}

// EnsureSession finds or creates a durable runtime session record.
func (m *RuntimeManager) EnsureSession(ctx context.Context, input RuntimeEnsureInput) (*RuntimeHandle, error) {
	return m.EnsureSessionObserved(ctx, input, nil)
}

// EnsureSessionObserved finds or creates a durable runtime session record and
// optionally mirrors raw ACP traffic during session bootstrap.
func (m *RuntimeManager) EnsureSessionObserved(ctx context.Context, input RuntimeEnsureInput, onRawMessage func(json.RawMessage)) (*RuntimeHandle, error) {
	agent := NormalizeAgentName(input.Agent)
	if agent == "" {
		return nil, fmt.Errorf("agent is required")
	}
	cwd := m.resolveCWD(input.CWD)
	name := normalizeSessionName(input.Name, cwd)
	mode := input.Mode
	if mode == "" {
		mode = RuntimeSessionModePersistent
	}
	sessionKey := strings.TrimSpace(input.SessionKey)
	if sessionKey == "" {
		sessionKey = scopedSessionKey(agent, cwd, name)
	}

	existing, err := m.store.FindByKey(sessionKey)
	if err != nil {
		return nil, err
	}
	if existing != nil && !existing.Closed && mode == RuntimeSessionModePersistent {
		return recordToHandle(existing), nil
	}

	client, err := m.newClient(agent, cwd, input.SessionOptions)
	if err != nil {
		return nil, err
	}
	client.SetRawMessageHandler(onRawMessage)
	defer client.Disconnect()
	if err := client.EnsureReady(ctx); err != nil {
		return nil, err
	}

	recordID := sessionKey + ":" + generateSessionID()
	record := &SessionRecord{
		RecordID:       recordID,
		SessionKey:     sessionKey,
		Backend:        "acp",
		Agent:          agent,
		Name:           name,
		CWD:            cwd,
		Mode:           string(mode),
		ConfigValues:   map[string]string{},
		HistoryPath:    filepath.Join(m.stateDir, "history", safeRecordFileName(recordID)+".ndjson"),
		SessionOptions: cloneSessionOptions(input.SessionOptions),
	}

	if input.ResumeSessionID != "" {
		if _, err := client.ResumeSession(ctx, input.ResumeSessionID, cwd); err != nil {
			return nil, err
		}
		record.BackendSessionID = input.ResumeSessionID
	} else {
		if err := client.NewSession(ctx, cwd); err != nil {
			return nil, err
		}
		record.BackendSessionID = client.CurrentSessionID()
	}
	if record.BackendSessionID == "" {
		return nil, fmt.Errorf("agent %q did not return a session id", agent)
	}
	applyClientSessionState(record, client)
	if err := m.store.Save(record); err != nil {
		return nil, err
	}
	return recordToHandle(record), nil
}

func (m *RuntimeManager) newClient(agent, cwd string, options *SessionOptions) (*Client, error) {
	target, err := m.registry.ResolveLaunchTarget(agent)
	if err != nil {
		return nil, fmt.Errorf("resolve agent %q: %w", agent, err)
	}
	return NewClientWithOptions(target, cwd, m.policy, m.mcpServers, ClientOptions{
		SessionOptions: cloneSessionOptions(options),
	}), nil
}

func (m *RuntimeManager) prepareClientForRecord(ctx context.Context, record *SessionRecord) (*Client, error) {
	return m.prepareClientForRecordObserved(ctx, record, nil)
}

func (m *RuntimeManager) prepareClientForRecordObserved(ctx context.Context, record *SessionRecord, onRawMessage func(json.RawMessage)) (*Client, error) {
	client, err := m.newClient(record.Agent, record.CWD, record.SessionOptions)
	if err != nil {
		return nil, err
	}
	client.SetRawMessageHandler(onRawMessage)
	if err := client.EnsureReady(ctx); err != nil {
		_ = client.Disconnect()
		return nil, err
	}
	if record.BackendSessionID != "" {
		if _, err := client.ResumeSession(ctx, record.BackendSessionID, record.CWD); err == nil {
			applyClientSessionState(record, client)
			return client, nil
		}
	}
	if err := client.NewSession(ctx, record.CWD); err != nil {
		_ = client.Disconnect()
		return nil, err
	}
	record.BackendSessionID = client.CurrentSessionID()
	applyClientSessionState(record, client)
	return client, nil
}

func (m *RuntimeManager) appendHistory(recordID string, entry SessionHistoryEntry) {
	if err := m.history.Append(recordID, entry); err != nil {
		debugLogf("append history for %s failed: %v", recordID, err)
	}
}

func promptEventToHistoryEntry(event PromptEvent) SessionHistoryEntry {
	switch event.Type {
	case PromptEventText:
		return SessionHistoryEntry{Kind: "text", Role: "assistant", Text: event.Text}
	case PromptEventToolCall:
		return SessionHistoryEntry{
			Kind:      "tool_call",
			ToolName:  event.ToolName,
			ToolID:    event.ToolID,
			ToolTitle: event.ToolTitle,
			ToolArgs:  event.ToolArgs,
		}
	case PromptEventToolResult:
		return SessionHistoryEntry{
			Kind:      "tool_result",
			ToolName:  event.ToolName,
			ToolID:    event.ToolID,
			ToolTitle: event.ToolTitle,
			ToolArgs:  event.ToolArgs,
			Text:      event.Result,
			IsError:   event.IsError,
		}
	default:
		return SessionHistoryEntry{Kind: string(event.Type)}
	}
}

func promptEventToRuntimeEvent(event PromptEvent) RuntimeEvent {
	switch event.Type {
	case PromptEventText:
		return RuntimeEvent{Type: RuntimeEventTextDelta, Text: event.Text}
	case PromptEventToolCall:
		return RuntimeEvent{
			Type:      RuntimeEventToolCall,
			ToolName:  event.ToolName,
			ToolID:    event.ToolID,
			ToolTitle: event.ToolTitle,
			ToolArgs:  event.ToolArgs,
		}
	case PromptEventToolResult:
		return RuntimeEvent{
			Type:      RuntimeEventToolResult,
			ToolName:  event.ToolName,
			ToolID:    event.ToolID,
			ToolTitle: event.ToolTitle,
			ToolArgs:  event.ToolArgs,
			Text:      event.Result,
			IsError:   event.IsError,
		}
	default:
		return RuntimeEvent{Type: RuntimeEventStatus, Status: string(event.Type)}
	}
}

func permissionEscalationToHistoryEntry(event PermissionEscalationEvent) SessionHistoryEntry {
	payload, _ := json.Marshal(event)
	return SessionHistoryEntry{
		Kind:      "permission_escalation",
		ToolName:  event.ToolName,
		ToolID:    event.ToolCallID,
		ToolTitle: event.ToolTitle,
		Metadata:  payload,
	}
}

func permissionEscalationToRuntimeEvent(event PermissionEscalationEvent) RuntimeEvent {
	return RuntimeEvent{
		Type:                 RuntimeEventPermissionEscalation,
		ToolName:             event.ToolName,
		ToolID:               event.ToolCallID,
		ToolTitle:            event.ToolTitle,
		PermissionEscalation: &event,
	}
}

// RunTurn executes a prompt turn against a durable session record.
func (m *RuntimeManager) RunTurn(ctx context.Context, input RuntimeTurnInput, onEvent func(RuntimeEvent)) (*RuntimeTurnResult, error) {
	return m.RunTurnObserved(ctx, input, RuntimeTurnCallbacks{OnEvent: onEvent})
}

// RunTurnWithCancel executes a prompt turn and listens for external cancel requests.
func (m *RuntimeManager) RunTurnWithCancel(ctx context.Context, input RuntimeTurnInput, cancel <-chan struct{}, onEvent func(RuntimeEvent)) (*RuntimeTurnResult, error) {
	return m.RunTurnWithCancelObserved(ctx, input, cancel, RuntimeTurnCallbacks{OnEvent: onEvent})
}

func (m *RuntimeManager) RunTurnObserved(ctx context.Context, input RuntimeTurnInput, callbacks RuntimeTurnCallbacks) (*RuntimeTurnResult, error) {
	return m.RunTurnWithCancelObserved(ctx, input, nil, callbacks)
}

func (m *RuntimeManager) RunTurnWithCancelObserved(ctx context.Context, input RuntimeTurnInput, cancel <-chan struct{}, callbacks RuntimeTurnCallbacks) (*RuntimeTurnResult, error) {
	record, err := m.store.Load(input.Handle.RecordID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, fmt.Errorf("session record %q not found", input.Handle.RecordID)
	}
	if record.Closed {
		return nil, fmt.Errorf("session %q is closed", input.Handle.RecordID)
	}

	client, err := m.prepareClientForRecordObserved(ctx, record, callbacks.OnRawMessage)
	if err != nil {
		record.LastError = err.Error()
		_ = m.store.Save(record)
		return nil, err
	}
	defer func() {
		if record.Mode == string(RuntimeSessionModePersistent) {
			_ = client.Disconnect()
			return
		}
		_ = client.Close()
	}()
	client.SetPermissionEscalationHandler(func(event PermissionEscalationEvent) {
		m.appendHistory(record.RecordID, permissionEscalationToHistoryEntry(event))
		if callbacks.OnEvent != nil {
			callbacks.OnEvent(permissionEscalationToRuntimeEvent(event))
		}
	})
	if callbacks.OnActiveController != nil {
		callbacks.OnActiveController(runtimeActiveSessionController{client: client})
		if callbacks.OnActiveControllerClosed != nil {
			defer callbacks.OnActiveControllerClosed()
		}
	}
	cancelDone := make(chan struct{})
	if cancel != nil {
		go func() {
			defer close(cancelDone)
			select {
			case <-cancel:
				client.CancelActivePrompt()
			case <-ctx.Done():
			}
		}()
	} else {
		close(cancelDone)
	}

	record.BackendSessionID = client.CurrentSessionID()
	m.appendHistory(record.RecordID, SessionHistoryEntry{
		Kind: "prompt",
		Role: "user",
		Text: input.Text,
	})

	result, err := client.PromptStream(ctx, input.Text, func(event PromptEvent) {
		m.appendHistory(record.RecordID, promptEventToHistoryEntry(event))
		if callbacks.OnEvent != nil {
			callbacks.OnEvent(promptEventToRuntimeEvent(event))
		}
	})
	now := time.Now().UTC()
	record.LastPrompt = input.Text
	record.LastPromptAt = &now
	record.BackendSessionID = client.CurrentSessionID()
	applyClientSessionState(record, client)
	if err != nil {
		record.LastError = err.Error()
		record.Summary = "prompt failed"
		_ = m.store.Save(record)
		<-cancelDone
		return nil, err
	}
	record.LastError = ""
	record.LastStopReason = string(result.StopReason)
	record.Summary = fmt.Sprintf("last prompt finished with %s", result.StopReason)
	applyClientSessionState(record, client)
	if record.Mode != string(RuntimeSessionModePersistent) {
		record.Closed = true
		record.ClosedAt = &now
	}
	if strings.TrimSpace(result.Text) != "" {
		m.appendHistory(record.RecordID, SessionHistoryEntry{
			Kind: "message",
			Role: "assistant",
			Text: result.Text,
		})
	}
	if err := m.store.Save(record); err != nil {
		<-cancelDone
		return nil, err
	}
	<-cancelDone
	return &RuntimeTurnResult{
		Text:       result.Text,
		StopReason: result.StopReason,
		Record:     record,
	}, nil
}

// RunOnce creates a short-lived session, executes a prompt, and closes it immediately.
func (m *RuntimeManager) RunOnce(ctx context.Context, agent, cwd, text string, onEvent func(RuntimeEvent)) (*RuntimeTurnResult, error) {
	return m.RunOnceWithOptionsObserved(ctx, agent, cwd, text, nil, RuntimeTurnCallbacks{OnEvent: onEvent})
}

// RunOnceWithOptions creates a short-lived session with explicit session options.
func (m *RuntimeManager) RunOnceWithOptions(ctx context.Context, agent, cwd, text string, sessionOptions *SessionOptions, onEvent func(RuntimeEvent)) (*RuntimeTurnResult, error) {
	return m.RunOnceWithOptionsObserved(ctx, agent, cwd, text, sessionOptions, RuntimeTurnCallbacks{OnEvent: onEvent})
}

func (m *RuntimeManager) RunOnceWithOptionsObserved(ctx context.Context, agent, cwd, text string, sessionOptions *SessionOptions, callbacks RuntimeTurnCallbacks) (*RuntimeTurnResult, error) {
	handle, err := m.EnsureSessionObserved(ctx, RuntimeEnsureInput{
		Agent:          agent,
		CWD:            cwd,
		Name:           "exec",
		Mode:           RuntimeSessionModeOneshot,
		SessionOptions: cloneSessionOptions(sessionOptions),
	}, callbacks.OnRawMessage)
	if err != nil {
		return nil, err
	}
	return m.RunTurnObserved(ctx, RuntimeTurnInput{Handle: *handle, Text: text}, callbacks)
}

// ListSessions returns locally persisted session records filtered by agent/name.
func (m *RuntimeManager) ListSessions(agent, name string, includeClosed bool) ([]*SessionRecord, error) {
	records, err := m.store.List()
	if err != nil {
		return nil, err
	}
	agent = NormalizeAgentName(agent)
	name = strings.TrimSpace(name)
	filtered := make([]*SessionRecord, 0, len(records))
	for _, record := range records {
		if agent != "" && record.Agent != agent {
			continue
		}
		if name != "" && record.Name != name {
			continue
		}
		if !includeClosed && record.Closed {
			continue
		}
		filtered = append(filtered, record)
	}
	return filtered, nil
}

// ListAgentSessions queries the remote agent for its current session index.
func (m *RuntimeManager) ListAgentSessions(ctx context.Context, agent, cwd, cursor string) (*ListSessionsResponse, error) {
	client, err := m.newClient(agent, m.resolveCWD(cwd), nil)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect()
	if err := client.EnsureReady(ctx); err != nil {
		return nil, err
	}
	return client.ListSessions(ctx, cursor, m.resolveCWD(cwd))
}

// Status returns the current local status for a persisted session.
func (m *RuntimeManager) Status(recordID string) (*RuntimeStatus, error) {
	record, err := m.store.Load(recordID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	status := &RuntimeStatus{
		RecordID:         record.RecordID,
		SessionKey:       record.SessionKey,
		Agent:            record.Agent,
		CWD:              record.CWD,
		Name:             record.Name,
		Title:            record.Title,
		BackendSessionID: record.BackendSessionID,
		Status:           "idle",
		Mode:             record.Mode,
		AvailableModes:   append([]string(nil), record.AvailableModes...),
		AvailableModels:  append([]string(nil), record.AvailableModels...),
		Closed:           record.Closed,
		Summary:          record.Summary,
		LastStopReason:   record.LastStopReason,
		LastPrompt:       record.LastPrompt,
		LastPromptAt:     record.LastPromptAt,
		LastError:        record.LastError,
		ActiveRequestID:  record.ActiveRequestID,
		OwnerPID:         record.OwnerPID,
		ConfigValues:     cloneConfigValues(record.ConfigValues),
	}
	queue := NewFileQueueStore(m.stateDir)
	if requests, err := queue.List(record.RecordID); err == nil {
		for _, request := range requests {
			switch request.Status {
			case QueueRequestQueued, QueueRequestRunning:
				status.QueueDepth++
			}
		}
		if status.QueueDepth > 0 {
			status.QueueState = "queued"
		}
	}
	if health, err := queue.ProbeOwner(record.RecordID); err == nil && health != nil {
		status.OwnerPID = health.PID
		status.OwnerAlive = health.PIDAlive
		status.OwnerHealthy = health.Healthy
		status.OwnerStartedAt = health.StartedAt
		if health.QueueDepth > status.QueueDepth {
			status.QueueDepth = health.QueueDepth
		}
		if health.Healthy {
			status.Status = "running"
			status.QueueState = "running"
			status.Uptime = formatRuntimeUptime(health.StartedAt)
		} else if health.PIDAlive || health.Stale {
			status.Status = "dead"
			if status.QueueState == "" {
				status.QueueState = "dead"
			}
		}
	}
	if status.QueueState == "" {
		status.QueueState = "idle"
	}
	if status.Status == "idle" && status.Closed {
		status.Status = "dead"
	}
	if strings.TrimSpace(status.Summary) == "" {
		status.Summary = summarizeRuntimeStatus(status.Status)
	}
	return status, nil
}

// Cancel asks the underlying agent session to cancel any active prompt work.
func (m *RuntimeManager) Cancel(ctx context.Context, recordID string) error {
	record, err := m.store.Load(recordID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("session record %q not found", recordID)
	}
	client, err := m.prepareClientForRecord(ctx, record)
	if err != nil {
		record.LastError = err.Error()
		_ = m.store.Save(record)
		return err
	}
	client.CancelActivePrompt()
	_ = client.Disconnect()
	record.Summary = "cancel requested"
	record.LastError = ""
	return m.store.Save(record)
}

// SetSessionMode updates the local record and the remote session mode.
func (m *RuntimeManager) SetSessionMode(ctx context.Context, recordID string, mode SessionModeId) error {
	record, err := m.store.Load(recordID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("session record %q not found", recordID)
	}
	record.Mode = string(mode)
	client, err := m.prepareClientForRecord(ctx, record)
	if err == nil {
		if setErr := client.SetSessionMode(ctx, mode); setErr != nil {
			err = setErr
		}
		applyClientSessionState(record, client)
		_ = client.Disconnect()
	}
	if err != nil {
		record.LastError = err.Error()
	} else {
		record.LastError = ""
	}
	return m.store.Save(record)
}

// SetSessionConfigOption updates the local record and remote config option state.
func (m *RuntimeManager) SetSessionConfigOption(ctx context.Context, recordID string, configID SessionConfigId, value SessionConfigValueId) error {
	record, err := m.store.Load(recordID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("session record %q not found", recordID)
	}
	if record.ConfigValues == nil {
		record.ConfigValues = make(map[string]string)
	}
	record.ConfigValues[string(configID)] = string(value)
	client, err := m.prepareClientForRecord(ctx, record)
	if err == nil {
		if _, setErr := client.SetSessionConfigOption(ctx, configID, value); setErr != nil {
			err = setErr
		}
		applyClientSessionState(record, client)
		_ = client.Disconnect()
	}
	if err != nil {
		record.LastError = err.Error()
	} else {
		record.LastError = ""
	}
	return m.store.Save(record)
}

// SetSessionModel updates the local record and remote model selector state.
func (m *RuntimeManager) SetSessionModel(ctx context.Context, recordID string, modelID string) error {
	record, err := m.store.Load(recordID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("session record %q not found", recordID)
	}
	if record.ConfigValues == nil {
		record.ConfigValues = make(map[string]string)
	}
	record.ConfigValues["model"] = modelID
	client, err := m.prepareClientForRecord(ctx, record)
	if err == nil {
		if setErr := client.SetSessionModel(ctx, modelID); setErr != nil {
			err = setErr
		}
		applyClientSessionState(record, client)
		_ = client.Disconnect()
	}
	if err != nil {
		record.LastError = err.Error()
	} else {
		record.LastError = ""
	}
	return m.store.Save(record)
}

// ReadHistory returns the durable history for a session.
func (m *RuntimeManager) ReadHistory(recordID string) ([]SessionHistoryEntry, error) {
	return m.history.Read(recordID)
}

// ExportSession writes a session archive to path.
func (m *RuntimeManager) ExportSession(recordID, path string) error {
	record, err := m.store.Load(recordID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("session record %q not found", recordID)
	}
	if health, err := NewFileQueueStore(m.stateDir).ProbeOwner(recordID); err == nil && health != nil && health.HasLease {
		return fmt.Errorf("session is currently locked by a running queue owner; close it first with `acp-go sessions close`")
	}
	history, err := m.history.Read(recordID)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(SessionExport{
		Record:  *record,
		History: history,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding session export: %w", err)
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}

// ImportSession loads a session archive into the local store.
func (m *RuntimeManager) ImportSession(path string) (*SessionRecord, error) {
	return m.ImportSessionWithOptions(path, ImportSessionOptions{})
}

// ImportSessionWithOptions loads a session archive with optional destination overrides.
func (m *RuntimeManager) ImportSessionWithOptions(path string, options ImportSessionOptions) (*SessionRecord, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading session export: %w", err)
	}
	var archive SessionExport
	if err := json.Unmarshal(payload, &archive); err != nil {
		return nil, fmt.Errorf("decoding session export: %w", err)
	}
	record := normalizeSessionRecord(&archive.Record)
	overrideScope := false
	if value := strings.TrimSpace(options.Name); value != "" {
		record.Name = value
		overrideScope = true
	}
	if value := strings.TrimSpace(options.CWD); value != "" {
		record.CWD = filepath.Clean(value)
		overrideScope = true
	}
	record.SessionKey = scopedSessionKey(record.Agent, record.CWD, record.Name)
	if overrideScope {
		record.RecordID = record.SessionKey + ":" + generateSessionID()
	} else if existing, err := m.store.Load(record.RecordID); err != nil {
		return nil, err
	} else if existing != nil {
		record.RecordID = record.SessionKey + ":" + generateSessionID()
	}
	record.HistoryPath = filepath.Join(m.stateDir, "history", safeRecordFileName(record.RecordID)+".ndjson")
	if err := m.store.Save(record); err != nil {
		return nil, err
	}
	if err := m.history.Replace(record.RecordID, archive.History); err != nil {
		return nil, err
	}
	return record, nil
}

// CloseSession marks a session closed and attempts to close the remote session if still reachable.
func (m *RuntimeManager) CloseSession(ctx context.Context, recordID string) (*SessionRecord, error) {
	record, err := m.store.Load(recordID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, fmt.Errorf("session record %q not found", recordID)
	}
	queue := NewFileQueueStore(m.stateDir)
	if health, healthErr := queue.ProbeOwner(recordID); healthErr == nil && health != nil && health.HasLease {
		if health.PID != os.Getpid() {
			if killErr := terminatePID(health.PID); killErr != nil {
				record.LastError = killErr.Error()
			}
			_ = queue.ClearLease(recordID)
		}
	}
	if !record.Closed {
		client, clientErr := m.prepareClientForRecord(ctx, record)
		if clientErr == nil {
			_ = client.Close()
		} else {
			record.LastError = clientErr.Error()
		}
		now := time.Now().UTC()
		record.Closed = true
		record.ClosedAt = &now
		record.Summary = "session closed"
	}
	if err := m.store.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

// PruneClosedSessions deletes closed session records and their histories.
func (m *RuntimeManager) PruneClosedSessions(olderThan time.Duration) ([]string, error) {
	result, err := m.PruneClosedSessionsWithOptions(PruneClosedSessionsOptions{
		OlderThan:      olderThan,
		IncludeHistory: true,
	})
	if err != nil {
		return nil, err
	}
	return result.Deleted, nil
}

// PruneClosedSessionsWithOptions deletes or previews closed-session cleanup.
func (m *RuntimeManager) PruneClosedSessionsWithOptions(options PruneClosedSessionsOptions) (*PruneClosedSessionsResult, error) {
	records, err := m.store.List()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	result := &PruneClosedSessionsResult{
		Records: make([]*SessionRecord, 0),
		Deleted: make([]string, 0),
		DryRun:  options.DryRun,
	}
	for _, record := range records {
		if !record.Closed {
			continue
		}
		if options.Before != nil {
			timestamp := record.UpdatedAt
			if record.ClosedAt != nil {
				timestamp = *record.ClosedAt
			}
			if !timestamp.Before(*options.Before) {
				continue
			}
		}
		if options.OlderThan > 0 {
			timestamp := record.UpdatedAt
			if record.ClosedAt != nil {
				timestamp = *record.ClosedAt
			}
			if now.Sub(timestamp) < options.OlderThan {
				continue
			}
		}
		result.Records = append(result.Records, record)
		if options.DryRun {
			result.Deleted = append(result.Deleted, record.RecordID)
			continue
		}
		if stat, statErr := os.Stat(record.HistoryPath); statErr == nil {
			result.BytesFreed += stat.Size()
		}
		if err := m.store.Delete(record.RecordID); err != nil {
			return nil, err
		}
		if options.IncludeHistory {
			if err := m.history.Delete(record.RecordID); err != nil {
				return nil, err
			}
		}
		result.Deleted = append(result.Deleted, record.RecordID)
	}
	sort.Strings(result.Deleted)
	return result, nil
}
