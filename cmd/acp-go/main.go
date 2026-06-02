package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	acp "github.com/topcheer/ggcode-acp-go"
)

type flowFile struct {
	Agent string     `json:"agent,omitempty"`
	CWD   string     `json:"cwd,omitempty"`
	Name  string     `json:"name,omitempty"`
	Steps []flowStep `json:"steps"`
}

type flowStep struct {
	Agent  string `json:"agent,omitempty"`
	CWD    string `json:"cwd,omitempty"`
	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	File   string `json:"file,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

const unsetListFlag = "\x00"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usage(stderr)
	}
	knownCommands := map[string]bool{
		"prompt": true, "exec": true, "status": true, "cancel": true,
		"set-mode": true, "set": true,
		"sessions": true, "config": true, "agents": true, "flow": true, "internal-owner": true,
	}
	agentHint := ""
	if !knownCommands[args[0]] && !strings.HasPrefix(args[0], "-") {
		agentHint = args[0]
		args = args[1:]
	}
	if len(args) == 0 {
		return usage(stderr)
	}

	switch args[0] {
	case "prompt":
		return runPrompt(args[1:], agentHint, stdout)
	case "exec":
		return runExec(args[1:], agentHint, stdout)
	case "status":
		return runStatus(args[1:], agentHint, stdout)
	case "cancel":
		return runCancel(args[1:], agentHint, stdout)
	case "set-mode":
		return runSetMode(args[1:], agentHint, stdout)
	case "set":
		return runSetConfig(args[1:], agentHint, stdout)
	case "sessions":
		return runSessions(args[1:], agentHint, stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "agents":
		return runAgents(args[1:], stdout)
	case "flow":
		return runFlow(args[1:], agentHint, stdout, stderr)
	case "internal-owner":
		return runInternalOwner(args[1:])
	default:
		return usage(stderr)
	}
}

func usage(w io.Writer) error {
	_, _ = fmt.Fprintln(w, "usage: acp-go [agent] <prompt|exec|status|cancel|set-mode|set|sessions|config|agents|flow> [...]")
	return fmt.Errorf("missing or unknown command")
}

func newManager(stateDir, cwd string) *acp.RuntimeManager {
	return acp.NewRuntimeManager(acp.RuntimeManagerOptions{
		StateDir:   stateDir,
		WorkingDir: cwd,
	})
}

func loadConfig(stateDir string) (*acp.RuntimeConfig, error) {
	return acp.NewFileConfigStore(stateDir).Load()
}

func saveConfig(stateDir string, cfg *acp.RuntimeConfig) error {
	return acp.NewFileConfigStore(stateDir).Save(cfg)
}

func chooseAgent(explicit, hint string, cfg *acp.RuntimeConfig) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if strings.TrimSpace(hint) != "" {
		return hint
	}
	if cfg != nil {
		return cfg.DefaultAgent
	}
	return ""
}

func chooseSessionName(explicit string, cfg *acp.RuntimeConfig) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if cfg != nil && strings.TrimSpace(cfg.DefaultSessionName) != "" {
		return cfg.DefaultSessionName
	}
	return ""
}

func printJSON(w io.Writer, value interface{}) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(payload))
	return err
}

func printJSONRPCError(w io.Writer, message, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = "unknown"
	}
	payload, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]interface{}{
			"code":    -32603,
			"message": message,
			"data": map[string]interface{}{
				"acpxCode":  "RUNTIME",
				"sessionId": sessionID,
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(payload))
	return err
}

func readPromptInput(positional []string, textFlag, fileFlag string) (string, error) {
	if strings.TrimSpace(textFlag) != "" {
		return textFlag, nil
	}
	if strings.TrimSpace(fileFlag) != "" {
		payload, err := os.ReadFile(fileFlag)
		if err != nil {
			return "", err
		}
		return string(payload), nil
	}
	if len(positional) > 0 {
		return strings.Join(positional, " "), nil
	}
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	if stat.Mode()&os.ModeCharDevice == 0 {
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(payload), nil
	}
	return "", fmt.Errorf("prompt text is required")
}

func parseAllowedTools(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{}, nil
	}
	parts := strings.Split(trimmed, ",")
	tools := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, fmt.Errorf("allowed tools must not contain empty entries")
		}
		tools = append(tools, value)
	}
	return tools, nil
}

func sessionOptionsFromFlags(model, allowedTools, systemPrompt, appendSystemPrompt string, maxTurns int) (*acp.SessionOptions, error) {
	options := &acp.SessionOptions{}
	if value := strings.TrimSpace(model); value != "" {
		options.Model = value
	}
	if allowedTools != unsetListFlag {
		parsed, err := parseAllowedTools(allowedTools)
		if err != nil {
			return nil, err
		}
		if parsed != nil {
			options.AllowedTools = parsed
		}
	}
	if maxTurns > 0 {
		options.MaxTurns = maxTurns
	}
	if strings.TrimSpace(systemPrompt) != "" && strings.TrimSpace(appendSystemPrompt) != "" {
		return nil, fmt.Errorf("use only one of --system-prompt or --append-system-prompt")
	}
	if value := strings.TrimSpace(systemPrompt); value != "" {
		options.SystemPrompt = value
	} else if value := strings.TrimSpace(appendSystemPrompt); value != "" {
		options.SystemPrompt = acp.SystemPromptOption{Append: value}
	}
	if options.Model == "" && options.MaxTurns == 0 && len(options.AllowedTools) == 0 && options.SystemPrompt == nil {
		return nil, nil
	}
	return options, nil
}

func resolveHandle(manager *acp.RuntimeManager, recordID, agent, cwd, name string, walkParents bool) (*acp.RuntimeHandle, error) {
	if strings.TrimSpace(recordID) != "" {
		record, err := manager.LoadRecord(recordID)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, fmt.Errorf("session record %q not found", recordID)
		}
		return &acp.RuntimeHandle{
			RecordID:         record.RecordID,
			SessionKey:       record.SessionKey,
			Agent:            record.Agent,
			CWD:              record.CWD,
			Name:             record.Name,
			BackendSessionID: record.BackendSessionID,
		}, nil
	}
	record, err := manager.FindSession(agent, cwd, name, walkParents)
	if err != nil {
		return nil, err
	}
	if record != nil {
		return &acp.RuntimeHandle{
			RecordID:         record.RecordID,
			SessionKey:       record.SessionKey,
			Agent:            record.Agent,
			CWD:              record.CWD,
			Name:             record.Name,
			BackendSessionID: record.BackendSessionID,
		}, nil
	}
	return nil, nil
}

func queueRequestTerminal(status acp.QueueRequestStatus) bool {
	switch status {
	case acp.QueueRequestCompleted, acp.QueueRequestFailed, acp.QueueRequestCancelled:
		return true
	default:
		return false
	}
}

func sessionShouldUseQueue(queue *acp.FileQueueStore, recordID string) bool {
	if health, err := queue.ProbeOwner(recordID); err == nil && health != nil && health.Healthy {
		return true
	}
	requests, err := queue.List(recordID)
	if err != nil {
		return false
	}
	for _, request := range requests {
		if request.Status == acp.QueueRequestQueued || request.Status == acp.QueueRequestRunning {
			return true
		}
	}
	return false
}

func waitForQueueRequest(queue *acp.FileQueueStore, recordID, requestID string, pollInterval time.Duration) (*acp.QueueRequestRecord, error) {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	for {
		record, err := queue.Load(recordID, requestID)
		if err != nil {
			return nil, err
		}
		if record != nil && queueRequestTerminal(record.Status) {
			return record, nil
		}
		time.Sleep(pollInterval)
	}
}

func historyEntryToRuntimeEvent(entry acp.SessionHistoryEntry) (acp.RuntimeEvent, bool) {
	switch entry.Kind {
	case "text":
		return acp.RuntimeEvent{Type: acp.RuntimeEventTextDelta, Text: entry.Text}, true
	case "tool_call":
		return acp.RuntimeEvent{
			Type:      acp.RuntimeEventToolCall,
			ToolName:  entry.ToolName,
			ToolID:    entry.ToolID,
			ToolTitle: entry.ToolTitle,
			ToolArgs:  entry.ToolArgs,
		}, true
	case "tool_result":
		return acp.RuntimeEvent{
			Type:      acp.RuntimeEventToolResult,
			ToolName:  entry.ToolName,
			ToolID:    entry.ToolID,
			ToolTitle: entry.ToolTitle,
			ToolArgs:  entry.ToolArgs,
			Text:      entry.Text,
			IsError:   entry.IsError,
		}, true
	case "permission_escalation":
		var event acp.PermissionEscalationEvent
		if len(entry.Metadata) == 0 || json.Unmarshal(entry.Metadata, &event) != nil {
			return acp.RuntimeEvent{}, false
		}
		return acp.RuntimeEvent{
			Type:                 acp.RuntimeEventPermissionEscalation,
			ToolName:             event.ToolName,
			ToolID:               event.ToolCallID,
			ToolTitle:            event.ToolTitle,
			PermissionEscalation: &event,
		}, true
	default:
		return acp.RuntimeEvent{}, false
	}
}

type promptRenderer struct {
	writer       io.Writer
	wroteAny     bool
	atLineStart  bool
	streamedText bool
}

func newPromptRenderer(writer io.Writer) *promptRenderer {
	return &promptRenderer{writer: writer, atLineStart: true}
}

func (r *promptRenderer) write(chunk string) {
	if chunk == "" {
		return
	}
	_, _ = io.WriteString(r.writer, chunk)
	r.wroteAny = true
	r.atLineStart = strings.HasSuffix(chunk, "\n")
}

func (r *promptRenderer) writeLine(line string) {
	r.write(line)
	r.write("\n")
}

func (r *promptRenderer) beginSection() {
	if !r.atLineStart {
		r.write("\n")
	}
	if r.wroteAny {
		r.write("\n")
	}
}

func (r *promptRenderer) render(event acp.RuntimeEvent) {
	switch event.Type {
	case acp.RuntimeEventTextDelta:
		if event.Text == "" {
			return
		}
		r.streamedText = true
		r.write(event.Text)
	case acp.RuntimeEventToolCall:
		r.beginSection()
		title := strings.TrimSpace(event.ToolTitle)
		if title == "" {
			title = strings.TrimSpace(event.ToolName)
		}
		if title == "" {
			title = strings.TrimSpace(event.ToolID)
		}
		r.writeLine(fmt.Sprintf("[tool] %s (running)", title))
		if args := strings.TrimSpace(event.ToolArgs); args != "" {
			r.writeLine("  input:")
			for _, line := range strings.Split(args, "\n") {
				r.writeLine("    " + line)
			}
		}
	case acp.RuntimeEventToolResult:
		r.beginSection()
		title := strings.TrimSpace(event.ToolTitle)
		if title == "" {
			title = strings.TrimSpace(event.ToolName)
		}
		if title == "" {
			title = strings.TrimSpace(event.ToolID)
		}
		status := "completed"
		if event.IsError {
			status = "failed"
		}
		r.writeLine(fmt.Sprintf("[tool] %s (%s)", title, status))
		if args := strings.TrimSpace(event.ToolArgs); args != "" {
			r.writeLine("  input:")
			for _, line := range strings.Split(args, "\n") {
				r.writeLine("    " + line)
			}
		}
		if output := strings.TrimSpace(event.Text); output != "" {
			r.writeLine("  output:")
			for _, line := range strings.Split(output, "\n") {
				r.writeLine("    " + line)
			}
		}
	case acp.RuntimeEventPermissionEscalation:
		if event.PermissionEscalation == nil {
			return
		}
		r.beginSection()
		r.writeLine("[permission] " + event.PermissionEscalation.Message)
		details := make([]string, 0, 6)
		if value := strings.TrimSpace(event.PermissionEscalation.SessionID); value != "" {
			details = append(details, "sessionId: "+value)
		}
		if value := strings.TrimSpace(event.PermissionEscalation.ToolCallID); value != "" {
			details = append(details, "toolCallId: "+value)
		}
		if value := strings.TrimSpace(event.PermissionEscalation.ToolName); value != "" {
			details = append(details, "toolName: "+value)
		}
		if value := strings.TrimSpace(event.PermissionEscalation.ToolTitle); value != "" {
			details = append(details, "toolTitle: "+value)
		}
		if value := strings.TrimSpace(string(event.PermissionEscalation.ToolInput)); value != "" {
			details = append(details, "toolInput: "+value)
		}
		if value := strings.TrimSpace(string(event.PermissionEscalation.ToolKind)); value != "" {
			details = append(details, "toolKind: "+value)
		}
		if value := strings.TrimSpace(event.PermissionEscalation.MatchedRule); value != "" {
			details = append(details, "matchedRule: "+value)
		}
		for _, detail := range details {
			r.writeLine("  " + detail)
		}
	}
}

func (r *promptRenderer) finish() {
	if !r.atLineStart {
		r.write("\n")
	}
}

func finalizePromptOutput(renderer *promptRenderer, resultText string) {
	if renderer == nil {
		return
	}
	if !renderer.streamedText && strings.TrimSpace(resultText) != "" {
		if renderer.wroteAny {
			renderer.beginSection()
		}
		renderer.write(resultText)
	}
	renderer.finish()
}

func emitRawJSONMessage(writer io.Writer, message json.RawMessage) {
	if len(message) == 0 {
		return
	}
	_, _ = writer.Write(message)
	_, _ = io.WriteString(writer, "\n")
}

func waitForQueuedPromptJSON(queue *acp.FileQueueStore, recordID, requestID string, pollInterval time.Duration, writer io.Writer) (*acp.QueueRequestRecord, error) {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	seenLines := 0
	flush := func() error {
		lines, err := queue.ReadRawMessages(recordID, requestID)
		if err != nil {
			return err
		}
		if seenLines > len(lines) {
			seenLines = len(lines)
		}
		for _, line := range lines[seenLines:] {
			emitRawJSONMessage(writer, json.RawMessage(line))
		}
		seenLines = len(lines)
		return nil
	}
	for {
		if err := flush(); err != nil {
			return nil, err
		}
		record, err := queue.Load(recordID, requestID)
		if err != nil {
			return nil, err
		}
		if record != nil && queueRequestTerminal(record.Status) {
			if err := flush(); err != nil {
				return nil, err
			}
			return record, nil
		}
		time.Sleep(pollInterval)
	}
}

func waitForQueuedPromptResult(manager *acp.RuntimeManager, queue *acp.FileQueueStore, recordID, requestID string, pollInterval time.Duration, onEvent func(acp.RuntimeEvent)) (*acp.QueueRequestRecord, error) {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	historySeen := 0
	var startedAt *time.Time
	for {
		record, err := queue.Load(recordID, requestID)
		if err != nil {
			return nil, err
		}
		if record != nil && record.StartedAt != nil && startedAt == nil {
			startedAt = record.StartedAt
		}
		if startedAt != nil && onEvent != nil {
			entries, err := manager.ReadHistory(recordID)
			if err != nil {
				return nil, err
			}
			if historySeen > len(entries) {
				historySeen = len(entries)
			}
			for _, entry := range entries[historySeen:] {
				if entry.Timestamp.Before(*startedAt) {
					continue
				}
				if event, ok := historyEntryToRuntimeEvent(entry); ok {
					onEvent(event)
				}
			}
			historySeen = len(entries)
		}
		if record != nil && queueRequestTerminal(record.Status) {
			return record, nil
		}
		time.Sleep(pollInterval)
	}
}

func runtimeResultFromQueueRequest(manager *acp.RuntimeManager, recordID string, request *acp.QueueRequestRecord) (*acp.RuntimeTurnResult, error) {
	record, err := manager.LoadRecord(recordID)
	if err != nil {
		return nil, err
	}
	return &acp.RuntimeTurnResult{
		Text:       request.ResultText,
		StopReason: acp.StopReason(request.StopReason),
		Record:     record,
	}, nil
}

func waitForQueueControl(queue *acp.FileQueueStore, recordID, requestID string, pollInterval time.Duration) (*acp.QueueRequestRecord, error) {
	completed, err := waitForQueueRequest(queue, recordID, requestID, pollInterval)
	if err != nil {
		return nil, err
	}
	if completed.Status == acp.QueueRequestFailed {
		return nil, fmt.Errorf("%s", completed.Error)
	}
	if completed.Status == acp.QueueRequestCancelled {
		return nil, fmt.Errorf("queue request %s was cancelled", requestID)
	}
	return completed, nil
}

func sessionHasActiveOwnerPrompt(status *acp.RuntimeStatus) bool {
	return status != nil && status.OwnerAlive && strings.TrimSpace(status.ActiveRequestID) != ""
}

func persistQueuedControlState(stateDir, recordID, modeID, modelID, configID, configValue string, closed bool) error {
	store := acp.NewFileSessionStore(stateDir)
	record, err := store.Load(recordID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("session record %q not found", recordID)
	}
	if strings.TrimSpace(modeID) != "" {
		record.Mode = modeID
		record.Summary = fmt.Sprintf("mode set to %s", modeID)
	}
	if strings.TrimSpace(modelID) != "" {
		if record.ConfigValues == nil {
			record.ConfigValues = make(map[string]string)
		}
		record.ConfigValues["model"] = modelID
		record.Summary = fmt.Sprintf("model set to %s", modelID)
	}
	if strings.TrimSpace(configID) != "" {
		if record.ConfigValues == nil {
			record.ConfigValues = make(map[string]string)
		}
		record.ConfigValues[configID] = configValue
		record.Summary = fmt.Sprintf("config set: %s=%s", configID, configValue)
	}
	if closed {
		now := time.Now().UTC()
		record.Closed = true
		record.ClosedAt = &now
		record.Summary = "session closed"
	}
	record.LastError = ""
	return store.Save(record)
}

func runPrompt(args []string, agentHint string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	text := fs.String("text", "", "")
	file := fs.String("file", "", "")
	model := fs.String("model", "", "")
	allowedTools := fs.String("allowed-tools", unsetListFlag, "")
	maxTurns := fs.Int("max-turns", 0, "")
	systemPrompt := fs.String("system-prompt", "", "")
	appendSystemPrompt := fs.String("append-system-prompt", "", "")
	jsonOutput := fs.Bool("json", false, "")
	walkParents := fs.Bool("walk-parents", true, "")
	waitForCompletion := fs.Bool("wait", true, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessionID := ""
	defer func() {
		if err != nil && *jsonOutput {
			_ = printJSONRPCError(stdout, err.Error(), sessionID)
			err = nil
		}
	}()

	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	selectedAgent := chooseAgent(*agent, agentHint, cfg)
	if selectedAgent == "" {
		return fmt.Errorf("agent is required")
	}
	sessionName := chooseSessionName(*name, cfg)
	promptText, err := readPromptInput(fs.Args(), *text, *file)
	if err != nil {
		return err
	}
	sessionOptions, err := sessionOptionsFromFlags(*model, *allowedTools, *systemPrompt, *appendSystemPrompt, *maxTurns)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	ctx := context.Background()
	var rawMu sync.Mutex
	rawOutput := func(message json.RawMessage) {
		rawMu.Lock()
		defer rawMu.Unlock()
		emitRawJSONMessage(stdout, message)
	}
	handle, err := resolveHandle(manager, *recordID, selectedAgent, *cwd, sessionName, *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		if *jsonOutput {
			handle, err = manager.EnsureSessionObserved(ctx, acp.RuntimeEnsureInput{
				Agent:          selectedAgent,
				CWD:            *cwd,
				Name:           sessionName,
				Mode:           acp.RuntimeSessionModePersistent,
				SessionOptions: sessionOptions,
			}, rawOutput)
		} else {
			handle, err = manager.EnsureSession(ctx, acp.RuntimeEnsureInput{
				Agent:          selectedAgent,
				CWD:            *cwd,
				Name:           sessionName,
				Mode:           acp.RuntimeSessionModePersistent,
				SessionOptions: sessionOptions,
			})
		}
		if err != nil {
			return err
		}
	}
	sessionID = handle.RecordID
	queue := acp.NewFileQueueStore(*stateDir)
	useQueue := sessionShouldUseQueue(queue, handle.RecordID)
	var renderer *promptRenderer
	if !*jsonOutput {
		renderer = newPromptRenderer(stdout)
	}
	if !*waitForCompletion || useQueue {
		var request *acp.QueueRequestRecord
		if *waitForCompletion && useQueue {
			streamResult, handled, streamErr := streamQueuedPromptViaOwnerSocket(*stateDir, handle.RecordID, promptText, *jsonOutput, stdout, func(event acp.RuntimeEvent) {
				if renderer == nil {
					return
				}
				renderer.render(event)
			})
			if streamResult != nil && streamResult.RequestID != "" {
				request = &acp.QueueRequestRecord{RequestID: streamResult.RequestID}
			}
			if handled && streamErr == nil {
				if *jsonOutput {
					return nil
				}
				finalizePromptOutput(renderer, streamResult.Text)
				return nil
			}
			if handled && streamErr != nil && request == nil {
				return streamErr
			}
		}
		if request == nil {
			request, err = queue.Enqueue(handle.RecordID, promptText)
			if err != nil {
				return err
			}
		}
		if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
			return err
		}
		if *jsonOutput {
			if !*waitForCompletion {
				return printJSON(stdout, request)
			}
		}
		if !*waitForCompletion {
			_, _ = fmt.Fprintf(stdout, "%s\n", request.RequestID)
			return nil
		}
		var completed *acp.QueueRequestRecord
		if *jsonOutput {
			completed, err = waitForQueuedPromptJSON(queue, handle.RecordID, request.RequestID, 500*time.Millisecond, stdout)
		} else {
			completed, err = waitForQueuedPromptResult(manager, queue, handle.RecordID, request.RequestID, 500*time.Millisecond, func(event acp.RuntimeEvent) {
				if renderer == nil {
					return
				}
				renderer.render(event)
			})
		}
		if err != nil {
			return err
		}
		if completed.Status == acp.QueueRequestFailed {
			return fmt.Errorf("%s", completed.Error)
		}
		if *jsonOutput {
			return nil
		}
		result, err := runtimeResultFromQueueRequest(manager, handle.RecordID, completed)
		if err != nil {
			return err
		}
		finalizePromptOutput(renderer, result.Text)
		return nil
	}
	var result *acp.RuntimeTurnResult
	if *jsonOutput {
		result, err = manager.RunTurnObserved(ctx, acp.RuntimeTurnInput{
			Handle: *handle,
			Text:   promptText,
		}, acp.RuntimeTurnCallbacks{OnRawMessage: rawOutput})
	} else {
		result, err = manager.RunTurn(ctx, acp.RuntimeTurnInput{
			Handle: *handle,
			Text:   promptText,
		}, func(event acp.RuntimeEvent) {
			if renderer == nil {
				return
			}
			renderer.render(event)
		})
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return nil
	}
	finalizePromptOutput(renderer, result.Text)
	return nil
}

func runExec(args []string, agentHint string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	text := fs.String("text", "", "")
	file := fs.String("file", "", "")
	model := fs.String("model", "", "")
	allowedTools := fs.String("allowed-tools", unsetListFlag, "")
	maxTurns := fs.Int("max-turns", 0, "")
	systemPrompt := fs.String("system-prompt", "", "")
	appendSystemPrompt := fs.String("append-system-prompt", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defer func() {
		if err != nil && *jsonOutput {
			_ = printJSONRPCError(stdout, err.Error(), "")
			err = nil
		}
	}()
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	selectedAgent := chooseAgent(*agent, agentHint, cfg)
	if selectedAgent == "" {
		return fmt.Errorf("agent is required")
	}
	promptText, err := readPromptInput(fs.Args(), *text, *file)
	if err != nil {
		return err
	}
	sessionOptions, err := sessionOptionsFromFlags(*model, *allowedTools, *systemPrompt, *appendSystemPrompt, *maxTurns)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	sawText := false
	var rawMu sync.Mutex
	rawOutput := func(message json.RawMessage) {
		rawMu.Lock()
		defer rawMu.Unlock()
		emitRawJSONMessage(stdout, message)
	}
	var result *acp.RuntimeTurnResult
	if *jsonOutput {
		result, err = manager.RunOnceWithOptionsObserved(context.Background(), selectedAgent, *cwd, promptText, sessionOptions, acp.RuntimeTurnCallbacks{
			OnRawMessage: rawOutput,
		})
	} else {
		result, err = manager.RunOnceWithOptions(context.Background(), selectedAgent, *cwd, promptText, sessionOptions, func(event acp.RuntimeEvent) {
			if event.Type == acp.RuntimeEventTextDelta && event.Text != "" {
				sawText = true
				_, _ = fmt.Fprint(stdout, event.Text)
			}
		})
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return nil
	}
	if !sawText && result.Text != "" {
		_, _ = fmt.Fprint(stdout, result.Text)
	}
	if result.Text != "" {
		_, _ = fmt.Fprintln(stdout)
	}
	return nil
}

func runStatus(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	jsonOutput := fs.Bool("json", false, "")
	walkParents := fs.Bool("walk-parents", true, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	selectedAgent := chooseAgent(*agent, agentHint, cfg)
	sessionName := chooseSessionName(*name, cfg)
	manager := newManager(*stateDir, *cwd)
	handle, err := resolveHandle(manager, *recordID, selectedAgent, *cwd, sessionName, *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		if *jsonOutput {
			return printJSON(stdout, map[string]interface{}{
				"action":  "status_snapshot",
				"status":  "no-session",
				"summary": "no active session",
			})
		}
		_, _ = fmt.Fprintln(stdout, "session: -")
		if selectedAgent == "" {
			selectedAgent = "-"
		}
		_, _ = fmt.Fprintf(stdout, "agent: %s\n", selectedAgent)
		_, _ = fmt.Fprintln(stdout, "pid: -")
		_, _ = fmt.Fprintln(stdout, "status: no-session")
		_, _ = fmt.Fprintln(stdout, "model: -")
		_, _ = fmt.Fprintln(stdout, "mode: -")
		_, _ = fmt.Fprintln(stdout, "uptime: -")
		_, _ = fmt.Fprintln(stdout, "lastPromptTime: -")
		return nil
	}
	status, err := manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, status)
	}
	_, _ = fmt.Fprintf(stdout, "session: %s\n", status.RecordID)
	_, _ = fmt.Fprintf(stdout, "agent: %s\n", status.Agent)
	if status.OwnerPID != 0 && status.OwnerAlive {
		_, _ = fmt.Fprintf(stdout, "pid: %d\n", status.OwnerPID)
	} else {
		_, _ = fmt.Fprintln(stdout, "pid: -")
	}
	_, _ = fmt.Fprintf(stdout, "status: %s\n", status.Status)
	if currentModel := strings.TrimSpace(status.ConfigValues["model"]); currentModel != "" {
		_, _ = fmt.Fprintf(stdout, "model: %s\n", currentModel)
	} else if len(status.AvailableModels) > 0 {
		_, _ = fmt.Fprintf(stdout, "model: %s\n", strings.Join(status.AvailableModels, ","))
	} else {
		_, _ = fmt.Fprintln(stdout, "model: -")
	}
	if status.Mode != "" {
		_, _ = fmt.Fprintf(stdout, "mode: %s\n", status.Mode)
	} else {
		_, _ = fmt.Fprintln(stdout, "mode: -")
	}
	if status.Uptime != "" {
		_, _ = fmt.Fprintf(stdout, "uptime: %s\n", status.Uptime)
	} else {
		_, _ = fmt.Fprintln(stdout, "uptime: -")
	}
	if status.LastPromptAt != nil {
		_, _ = fmt.Fprintf(stdout, "lastPromptTime: %s\n", status.LastPromptAt.Format(time.RFC3339))
	} else {
		_, _ = fmt.Fprintln(stdout, "lastPromptTime: -")
	}
	if status.Title != "" {
		_, _ = fmt.Fprintf(stdout, "title=%s\n", status.Title)
	}
	if status.BackendSessionID != "" {
		_, _ = fmt.Fprintf(stdout, "backendSessionId=%s\n", status.BackendSessionID)
	}
	if len(status.AvailableModes) > 0 {
		_, _ = fmt.Fprintf(stdout, "availableModes=%s\n", strings.Join(status.AvailableModes, ","))
	}
	if len(status.AvailableModels) > 0 {
		_, _ = fmt.Fprintf(stdout, "availableModels=%s\n", strings.Join(status.AvailableModels, ","))
	}
	if status.LastPrompt != "" {
		_, _ = fmt.Fprintf(stdout, "lastPrompt=%s\n", status.LastPrompt)
	}
	if status.LastStopReason != "" {
		_, _ = fmt.Fprintf(stdout, "lastStopReason=%s\n", status.LastStopReason)
	}
	if status.Summary != "" {
		_, _ = fmt.Fprintf(stdout, "summary=%s\n", status.Summary)
	}
	if status.LastError != "" {
		_, _ = fmt.Fprintf(stdout, "lastError=%s\n", status.LastError)
	}
	if status.QueueDepth > 0 {
		_, _ = fmt.Fprintf(stdout, "queueDepth=%d\n", status.QueueDepth)
	}
	if status.OwnerPID != 0 {
		_, _ = fmt.Fprintf(stdout, "ownerPid=%d ownerAlive=%t\n", status.OwnerPID, status.OwnerAlive)
	}
	if len(status.ConfigValues) > 0 {
		keys := make([]string, 0, len(status.ConfigValues))
		for key := range status.ConfigValues {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = fmt.Fprintf(stdout, "config.%s=%s\n", key, status.ConfigValues[key])
		}
	}
	return nil
}

func runCancel(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	jsonOutput := fs.Bool("json", false, "")
	walkParents := fs.Bool("walk-parents", true, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	selectedAgent := chooseAgent(*agent, agentHint, cfg)
	sessionName := chooseSessionName(*name, cfg)
	manager := newManager(*stateDir, *cwd)
	handle, err := resolveHandle(manager, *recordID, selectedAgent, *cwd, sessionName, *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	queue := acp.NewFileQueueStore(*stateDir)
	status, err := manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	targetRequestID := ""
	cancelled := false
	liveCancelHandled := false
	if status != nil && status.ActiveRequestID != "" {
		targetRequestID = status.ActiveRequestID
		if status.OwnerAlive {
			handled, liveCancelled, err := requestOwnerSocketCancel(*stateDir, handle.RecordID, targetRequestID)
			if err != nil {
				return err
			}
			if handled {
				cancelled = liveCancelled
				liveCancelHandled = true
			}
		}
	} else if requests, listErr := queue.List(handle.RecordID); listErr == nil {
		for i := len(requests) - 1; i >= 0; i-- {
			if requests[i].Status == acp.QueueRequestQueued || requests[i].Status == acp.QueueRequestRunning {
				targetRequestID = requests[i].RequestID
				break
			}
		}
	}
	if targetRequestID != "" && !liveCancelHandled {
		if _, err := queue.RequestCancel(handle.RecordID, targetRequestID); err != nil {
			return err
		}
		cancelled = true
	}
	if targetRequestID == "" {
		if status != nil && status.OwnerAlive {
			cancelled = false
		} else if err := manager.Cancel(context.Background(), handle.RecordID); err != nil {
			return err
		} else {
			cancelled = true
		}
	}
	status, err = manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, map[string]interface{}{
			"action":    "cancel_result",
			"recordId":  handle.RecordID,
			"cancelled": cancelled,
			"status":    status,
		})
	}
	if cancelled {
		_, _ = fmt.Fprintln(stdout, "cancel requested")
	} else {
		_, _ = fmt.Fprintln(stdout, "nothing to cancel")
	}
	return nil
}

func runSetMode(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("set-mode", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("session", "", "")
	recordID := fs.String("record-id", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("mode is required")
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), *cwd, chooseSessionName(*name, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	modeID := strings.TrimSpace(fs.Arg(0))
	queue := acp.NewFileQueueStore(*stateDir)
	status, err := manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if sessionShouldUseQueue(queue, handle.RecordID) {
		if status != nil && status.OwnerAlive {
			handled, err := requestOwnerSocketSetMode(*stateDir, handle.RecordID, modeID)
			if err != nil {
				return err
			}
			if !handled {
				request, err := queue.EnqueueSetMode(handle.RecordID, modeID)
				if err != nil {
					return err
				}
				if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
					return err
				}
				if sessionHasActiveOwnerPrompt(status) {
					if err := persistQueuedControlState(*stateDir, handle.RecordID, modeID, "", "", "", false); err != nil {
						return err
					}
				} else if _, err := waitForQueueControl(queue, handle.RecordID, request.RequestID, 500*time.Millisecond); err != nil {
					return err
				}
			}
		} else {
			request, err := queue.EnqueueSetMode(handle.RecordID, modeID)
			if err != nil {
				return err
			}
			if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
				return err
			}
			if sessionHasActiveOwnerPrompt(status) {
				if err := persistQueuedControlState(*stateDir, handle.RecordID, modeID, "", "", "", false); err != nil {
					return err
				}
			} else if _, err := waitForQueueControl(queue, handle.RecordID, request.RequestID, 500*time.Millisecond); err != nil {
				return err
			}
		}
	} else if err := manager.SetSessionMode(context.Background(), handle.RecordID, acp.SessionModeId(modeID)); err != nil {
		return err
	}
	status, err = manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, map[string]interface{}{
			"action":         "mode_set",
			"modeId":         modeID,
			"recordId":       handle.RecordID,
			"backendSession": status.BackendSessionID,
		})
	}
	_, _ = fmt.Fprintf(stdout, "mode set: %s\n", status.Mode)
	return nil
}

func runSetConfig(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("session", "", "")
	recordID := fs.String("record-id", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("config id and value are required")
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), *cwd, chooseSessionName(*name, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	configID := strings.TrimSpace(fs.Arg(0))
	valueID := strings.TrimSpace(fs.Arg(1))
	queue := acp.NewFileQueueStore(*stateDir)
	status, err := manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if sessionShouldUseQueue(queue, handle.RecordID) {
		if status != nil && status.OwnerAlive {
			var handled bool
			if configID == "model" {
				handled, err = requestOwnerSocketSetModel(*stateDir, handle.RecordID, valueID)
			} else {
				handled, err = requestOwnerSocketSetConfig(*stateDir, handle.RecordID, configID, valueID)
			}
			if err != nil {
				return err
			}
			if !handled {
				var request *acp.QueueRequestRecord
				if configID == "model" {
					request, err = queue.EnqueueSetModel(handle.RecordID, valueID)
				} else {
					request, err = queue.EnqueueSetConfig(handle.RecordID, configID, valueID)
				}
				if err != nil {
					return err
				}
				if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
					return err
				}
				if sessionHasActiveOwnerPrompt(status) {
					if configID == "model" {
						if err := persistQueuedControlState(*stateDir, handle.RecordID, "", valueID, "", "", false); err != nil {
							return err
						}
					} else if err := persistQueuedControlState(*stateDir, handle.RecordID, "", "", configID, valueID, false); err != nil {
						return err
					}
				} else if _, err := waitForQueueControl(queue, handle.RecordID, request.RequestID, 500*time.Millisecond); err != nil {
					return err
				}
			}
		} else {
			var request *acp.QueueRequestRecord
			if configID == "model" {
				request, err = queue.EnqueueSetModel(handle.RecordID, valueID)
			} else {
				request, err = queue.EnqueueSetConfig(handle.RecordID, configID, valueID)
			}
			if err != nil {
				return err
			}
			if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
				return err
			}
			if sessionHasActiveOwnerPrompt(status) {
				if configID == "model" {
					if err := persistQueuedControlState(*stateDir, handle.RecordID, "", valueID, "", "", false); err != nil {
						return err
					}
				} else if err := persistQueuedControlState(*stateDir, handle.RecordID, "", "", configID, valueID, false); err != nil {
					return err
				}
			} else if _, err := waitForQueueControl(queue, handle.RecordID, request.RequestID, 500*time.Millisecond); err != nil {
				return err
			}
		}
	} else {
		if configID == "model" {
			if err := manager.SetSessionModel(context.Background(), handle.RecordID, valueID); err != nil {
				return err
			}
		} else if err := manager.SetSessionConfigOption(context.Background(), handle.RecordID, acp.SessionConfigId(configID), acp.SessionConfigValueId(valueID)); err != nil {
			return err
		}
	}
	status, err = manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if *jsonOutput {
		action := "config_set"
		if configID == "model" {
			action = "model_set"
		}
		return printJSON(stdout, map[string]interface{}{
			"action":         action,
			"configId":       configID,
			"value":          valueID,
			"recordId":       handle.RecordID,
			"backendSession": status.BackendSessionID,
			"configValues":   status.ConfigValues,
		})
	}
	if configID == "model" {
		_, _ = fmt.Fprintf(stdout, "model set: %s\n", valueID)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "config set: %s=%s\n", configID, valueID)
	return nil
}

func runSessions(args []string, agentHint string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return runSessionsList(nil, agentHint, stdout)
	}
	switch args[0] {
	case "new":
		return runSessionsNew(args[1:], agentHint, stdout)
	case "ensure":
		return runSessionsEnsure(args[1:], agentHint, stdout)
	case "list":
		return runSessionsList(args[1:], agentHint, stdout)
	case "show":
		return runSessionsShow(args[1:], agentHint, stdout)
	case "history":
		return runSessionsHistory(args[1:], agentHint, stdout)
	case "read":
		return runSessionsRead(args[1:], agentHint, stdout)
	case "export":
		return runSessionsExport(args[1:], agentHint, stdout)
	case "import":
		return runSessionsImport(args[1:], stdout)
	case "close":
		return runSessionsClose(args[1:], agentHint, stdout)
	case "prune":
		return runSessionsPrune(args[1:], stdout)
	default:
		_, _ = fmt.Fprintln(stderr, "sessions subcommands: new, ensure, list, show, history, read, export, import, close, prune")
		return fmt.Errorf("unknown sessions subcommand")
	}
}

func runSessionsNew(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	resumeSessionID := fs.String("resume-session", "", "")
	model := fs.String("model", "", "")
	allowedTools := fs.String("allowed-tools", unsetListFlag, "")
	maxTurns := fs.Int("max-turns", 0, "")
	systemPrompt := fs.String("system-prompt", "", "")
	appendSystemPrompt := fs.String("append-system-prompt", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	sessionOptions, err := sessionOptionsFromFlags(*model, *allowedTools, *systemPrompt, *appendSystemPrompt, *maxTurns)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	selectedAgent := chooseAgent(*agent, agentHint, cfg)
	sessionName := chooseSessionName(*name, cfg)
	if existing, err := manager.FindSession(selectedAgent, *cwd, sessionName, false); err == nil && existing != nil && !existing.Closed {
		if _, closeErr := manager.CloseSession(context.Background(), existing.RecordID); closeErr != nil {
			return closeErr
		}
	} else if err != nil {
		return err
	}
	handle, err := manager.EnsureSession(context.Background(), acp.RuntimeEnsureInput{
		Agent:           selectedAgent,
		CWD:             *cwd,
		Name:            sessionName,
		ResumeSessionID: *resumeSessionID,
		Mode:            acp.RuntimeSessionModePersistent,
		SessionOptions:  sessionOptions,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, handle)
	}
	_, _ = fmt.Fprintln(stdout, handle.RecordID)
	return nil
}

func runSessionsEnsure(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions ensure", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	sessionKey := fs.String("session-key", "", "")
	resumeSessionID := fs.String("resume-session-id", "", "")
	mode := fs.String("mode", string(acp.RuntimeSessionModePersistent), "")
	model := fs.String("model", "", "")
	allowedTools := fs.String("allowed-tools", unsetListFlag, "")
	maxTurns := fs.Int("max-turns", 0, "")
	systemPrompt := fs.String("system-prompt", "", "")
	appendSystemPrompt := fs.String("append-system-prompt", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	sessionOptions, err := sessionOptionsFromFlags(*model, *allowedTools, *systemPrompt, *appendSystemPrompt, *maxTurns)
	if err != nil {
		return err
	}
	handle, err := newManager(*stateDir, *cwd).EnsureSession(context.Background(), acp.RuntimeEnsureInput{
		Agent:           chooseAgent(*agent, agentHint, cfg),
		CWD:             *cwd,
		Name:            chooseSessionName(*name, cfg),
		SessionKey:      *sessionKey,
		ResumeSessionID: *resumeSessionID,
		Mode:            acp.RuntimeSessionMode(*mode),
		SessionOptions:  sessionOptions,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, handle)
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", handle.RecordID)
	return nil
}

func runSessionsList(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", agentHint, "")
	name := fs.String("name", "", "")
	includeClosed := fs.Bool("closed", true, "")
	local := fs.Bool("local", false, "")
	cursor := fs.String("cursor", "", "")
	filterCWD := fs.String("filter-cwd", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	if *local {
		records, err := manager.ListSessions(*agent, *name, *includeClosed)
		if err != nil {
			return err
		}
		if strings.TrimSpace(*filterCWD) != "" {
			resolvedFilter := *filterCWD
			if abs, err := filepath.Abs(resolvedFilter); err == nil {
				resolvedFilter = abs
			}
			filtered := records[:0]
			for _, record := range records {
				if filepath.Clean(record.CWD) == filepath.Clean(resolvedFilter) {
					filtered = append(filtered, record)
				}
			}
			records = filtered
		}
		if *jsonOutput {
			return printJSON(stdout, records)
		}
		for _, record := range records {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\tname=%s\tclosed=%t\t%s\n", record.RecordID, record.Agent, record.CWD, record.Name, record.Closed, record.Summary)
		}
		return nil
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	selectedAgent := chooseAgent(*agent, agentHint, cfg)
	if selectedAgent == "" {
		return fmt.Errorf("agent is required for remote session listing")
	}
	response, err := manager.ListAgentSessions(context.Background(), selectedAgent, *cwd, *cursor)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*filterCWD) != "" {
		filtered := make([]acp.SessionInfo, 0, len(response.Sessions))
		resolvedFilter := *filterCWD
		if abs, err := filepath.Abs(resolvedFilter); err == nil {
			resolvedFilter = abs
		}
		for _, session := range response.Sessions {
			if filepath.Clean(session.CWD) == filepath.Clean(resolvedFilter) {
				filtered = append(filtered, session)
			}
		}
		response.Sessions = filtered
	}
	if *jsonOutput {
		return printJSON(stdout, response)
	}
	for _, session := range response.Sessions {
		_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", session.SessionID, session.Title, session.CWD, session.UpdatedAt)
	}
	return nil
}

func runSessionsShow(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	jsonOutput := fs.Bool("json", true, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	sessionName := *name
	if strings.TrimSpace(sessionName) == "" && fs.NArg() > 0 {
		sessionName = strings.TrimSpace(fs.Arg(0))
	}
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), *cwd, chooseSessionName(sessionName, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	record, err := manager.LoadRecord(handle.RecordID)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, record)
	}
	_, _ = fmt.Fprintf(stdout, "%+v\n", *record)
	return nil
}

func runSessionsHistory(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	limit := fs.Int("limit", 20, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	sessionName := *name
	if strings.TrimSpace(sessionName) == "" && fs.NArg() > 0 {
		sessionName = strings.TrimSpace(fs.Arg(0))
	}
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), *cwd, chooseSessionName(sessionName, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	entries, err := manager.ReadHistory(handle.RecordID)
	if err != nil {
		return err
	}
	if *limit > 0 && len(entries) > *limit {
		entries = entries[len(entries)-*limit:]
	}
	if *jsonOutput {
		return printJSON(stdout, entries)
	}
	for _, entry := range entries {
		_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", entry.Timestamp.Format(time.RFC3339), entry.Kind, entry.Role, strings.TrimSpace(entry.Text))
	}
	return nil
}

func runSessionsRead(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	tail := fs.Int("tail", 0, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	sessionName := *name
	if strings.TrimSpace(sessionName) == "" && fs.NArg() > 0 {
		sessionName = strings.TrimSpace(fs.Arg(0))
	}
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), *cwd, chooseSessionName(sessionName, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	entries, err := manager.ReadHistory(handle.RecordID)
	if err != nil {
		return err
	}
	if *tail > 0 && len(entries) > *tail {
		entries = entries[len(entries)-*tail:]
	}
	if *jsonOutput {
		return printJSON(stdout, entries)
	}
	for _, entry := range entries {
		_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", entry.Timestamp.Format(time.RFC3339), entry.Kind, entry.Role, strings.TrimSpace(entry.Text))
	}
	return nil
}

func runSessionsExport(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	outputPath := fs.String("output", "", "")
	sessionCWD := fs.String("cwd", "", "")
	sourceCWD := fs.String("source-cwd", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*outputPath) == "" {
		return fmt.Errorf("output path is required")
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	resolveCWD := *cwd
	if strings.TrimSpace(*sessionCWD) != "" {
		resolveCWD = *sessionCWD
	}
	if strings.TrimSpace(*sourceCWD) != "" {
		if filepath.IsAbs(*sourceCWD) {
			resolveCWD = *sourceCWD
		} else {
			resolveCWD = filepath.Join(*cwd, *sourceCWD)
		}
	}
	sessionName := *name
	if strings.TrimSpace(sessionName) == "" && fs.NArg() > 0 {
		sessionName = strings.TrimSpace(fs.Arg(0))
	}
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), resolveCWD, chooseSessionName(sessionName, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	if err := manager.ExportSession(handle.RecordID, *outputPath); err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, map[string]interface{}{
			"action": "session_exported",
			"output": *outputPath,
		})
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", *outputPath)
	return nil
}

func runSessionsImport(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	inputPath := fs.String("input", "", "")
	name := fs.String("name", "", "")
	destinationCWD := fs.String("cwd", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	archivePath := strings.TrimSpace(*inputPath)
	if archivePath == "" && fs.NArg() > 0 {
		archivePath = strings.TrimSpace(fs.Arg(0))
	}
	if archivePath == "" {
		return fmt.Errorf("input path is required")
	}
	record, err := newManager(*stateDir, "").ImportSessionWithOptions(archivePath, acp.ImportSessionOptions{
		Name: *name,
		CWD:  *destinationCWD,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, map[string]interface{}{
			"action":   "session_imported",
			"recordId": record.RecordID,
			"cwd":      record.CWD,
		})
	}
	_, _ = fmt.Fprintf(stdout, "imported session %s at %s\n", record.RecordID, record.CWD)
	return nil
}

func runSessionsClose(args []string, agentHint string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions close", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	recordID := fs.String("record-id", "", "")
	walkParents := fs.Bool("walk-parents", true, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	manager := newManager(*stateDir, *cwd)
	sessionName := *name
	if strings.TrimSpace(sessionName) == "" && fs.NArg() > 0 {
		sessionName = strings.TrimSpace(fs.Arg(0))
	}
	handle, err := resolveHandle(manager, *recordID, chooseAgent(*agent, agentHint, cfg), *cwd, chooseSessionName(sessionName, cfg), *walkParents)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("no matching session found")
	}
	queue := acp.NewFileQueueStore(*stateDir)
	var record *acp.SessionRecord
	status, err := manager.Status(handle.RecordID)
	if err != nil {
		return err
	}
	if sessionShouldUseQueue(queue, handle.RecordID) {
		if status != nil && status.OwnerAlive {
			handled, closed, err := requestOwnerSocketClose(*stateDir, handle.RecordID)
			if err != nil {
				return err
			}
			if handled {
				if !closed {
					return fmt.Errorf("owner did not close session %q", handle.RecordID)
				}
				record, err = manager.LoadRecord(handle.RecordID)
				if err != nil {
					return err
				}
				if record == nil {
					return fmt.Errorf("session record %q not found", handle.RecordID)
				}
			} else {
				if sessionHasActiveOwnerPrompt(status) {
					if _, err := queue.RequestCancel(handle.RecordID, status.ActiveRequestID); err != nil {
						return err
					}
				}
				request, err := queue.EnqueueClose(handle.RecordID)
				if err != nil {
					return err
				}
				if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
					return err
				}
				if _, err := waitForQueueControl(queue, handle.RecordID, request.RequestID, 500*time.Millisecond); err != nil {
					return err
				}
				record, err = manager.LoadRecord(handle.RecordID)
				if err != nil {
					return err
				}
				if record == nil {
					return fmt.Errorf("session record %q not found", handle.RecordID)
				}
			}
		} else {
			if sessionHasActiveOwnerPrompt(status) {
				if _, err := queue.RequestCancel(handle.RecordID, status.ActiveRequestID); err != nil {
					return err
				}
			}
			request, err := queue.EnqueueClose(handle.RecordID)
			if err != nil {
				return err
			}
			if err := ensureOwnerProcess(*stateDir, handle.RecordID); err != nil {
				return err
			}
			if _, err := waitForQueueControl(queue, handle.RecordID, request.RequestID, 500*time.Millisecond); err != nil {
				return err
			}
			record, err = manager.LoadRecord(handle.RecordID)
			if err != nil {
				return err
			}
			if record == nil {
				return fmt.Errorf("session record %q not found", handle.RecordID)
			}
		}
	} else {
		record, err = manager.CloseSession(context.Background(), handle.RecordID)
		if err != nil {
			return err
		}
	}
	if *jsonOutput {
		return printJSON(stdout, map[string]interface{}{
			"action":      "session_closed",
			"recordId":    record.RecordID,
			"sessionId":   record.BackendSessionID,
			"agent":       record.Agent,
			"closed":      record.Closed,
			"closedAt":    record.ClosedAt,
			"sessionName": record.Name,
		})
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", record.RecordID)
	return nil
}

func runSessionsPrune(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sessions prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	olderThan := fs.String("older-than", "", "")
	before := fs.String("before", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	includeHistory := fs.Bool("include-history", false, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var olderThanDuration time.Duration
	if strings.TrimSpace(*olderThan) != "" {
		if dayCount, err := strconv.ParseFloat(*olderThan, 64); err == nil {
			olderThanDuration = time.Duration(dayCount * float64(24*time.Hour))
		} else if duration, err := time.ParseDuration(*olderThan); err == nil {
			olderThanDuration = duration
		} else {
			return fmt.Errorf("older-than must be days or duration: %w", err)
		}
	}
	var beforeTime *time.Time
	if strings.TrimSpace(*before) != "" {
		parsed, err := time.Parse(time.RFC3339, *before)
		if err != nil {
			return fmt.Errorf("before must be RFC3339 timestamp: %w", err)
		}
		beforeTime = &parsed
	}
	result, err := newManager(*stateDir, "").PruneClosedSessionsWithOptions(acp.PruneClosedSessionsOptions{
		OlderThan:      olderThanDuration,
		Before:         beforeTime,
		IncludeHistory: *includeHistory,
		DryRun:         *dryRun,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		action := "sessions_pruned"
		if result.DryRun {
			action = "sessions_prune_dry_run"
		}
		return printJSON(stdout, map[string]interface{}{
			"action":     action,
			"dryRun":     result.DryRun,
			"count":      len(result.Deleted),
			"bytesFreed": result.BytesFreed,
			"pruned":     result.Deleted,
		})
	}
	if len(result.Deleted) == 0 {
		if result.DryRun {
			_, _ = fmt.Fprintln(stdout, "[DRY RUN] No sessions to prune")
		} else {
			_, _ = fmt.Fprintln(stdout, "No sessions pruned")
		}
		return nil
	}
	for _, recordID := range result.Deleted {
		_, _ = fmt.Fprintln(stdout, recordID)
	}
	return nil
}

func runConfig(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("config subcommand is required")
	}
	switch args[0] {
	case "show":
		fs := flag.NewFlagSet("config show", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
		jsonOutput := fs.Bool("json", false, "")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := loadConfig(*stateDir)
		if err != nil {
			return err
		}
		payload := map[string]interface{}{
			"stateDir": *stateDir,
			"config":   cfg,
			"agents":   acp.NewStaticAgentRegistry(nil).List(),
		}
		if *jsonOutput {
			return printJSON(stdout, payload)
		}
		_, _ = fmt.Fprintf(stdout, "stateDir=%s\ndefaultAgent=%s\ndefaultSessionName=%s\n", *stateDir, cfg.DefaultAgent, cfg.DefaultSessionName)
		return nil
	case "set-default-agent":
		fs := flag.NewFlagSet("config set-default-agent", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("default agent value is required")
		}
		cfg, err := loadConfig(*stateDir)
		if err != nil {
			return err
		}
		cfg.DefaultAgent = fs.Arg(0)
		return saveConfig(*stateDir, cfg)
	case "set-default-session":
		fs := flag.NewFlagSet("config set-default-session", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("default session value is required")
		}
		cfg, err := loadConfig(*stateDir)
		if err != nil {
			return err
		}
		cfg.DefaultSessionName = fs.Arg(0)
		return saveConfig(*stateDir, cfg)
	default:
		_, _ = fmt.Fprintln(stderr, "config subcommands: show, set-default-agent, set-default-session")
		return fmt.Errorf("unknown config subcommand")
	}
}

func runAgents(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	agents := acp.NewStaticAgentRegistry(nil).List()
	if *jsonOutput {
		return printJSON(stdout, agents)
	}
	for _, agent := range agents {
		_, _ = fmt.Fprintln(stdout, agent)
	}
	return nil
}

func runFlow(args []string, agentHint string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "run" {
		_, _ = fmt.Fprintln(stderr, "flow subcommands: run")
		return fmt.Errorf("unknown flow subcommand")
	}
	fs := flag.NewFlagSet("flow run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	cwd := fs.String("cwd", "", "")
	agent := fs.String("agent", "", "")
	name := fs.String("name", "", "")
	file := fs.String("file", "", "")
	model := fs.String("model", "", "")
	allowedTools := fs.String("allowed-tools", unsetListFlag, "")
	maxTurns := fs.Int("max-turns", 0, "")
	systemPrompt := fs.String("system-prompt", "", "")
	appendSystemPrompt := fs.String("append-system-prompt", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*file) == "" {
		return fmt.Errorf("flow file is required")
	}
	payload, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var flow flowFile
	if err := json.Unmarshal(payload, &flow); err != nil {
		return fmt.Errorf("decoding flow file: %w", err)
	}
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	sessionOptions, err := sessionOptionsFromFlags(*model, *allowedTools, *systemPrompt, *appendSystemPrompt, *maxTurns)
	if err != nil {
		return err
	}
	defaultAgent := chooseAgent(*agent, agentHint, cfg)
	if flow.Agent != "" {
		defaultAgent = flow.Agent
	}
	defaultName := chooseSessionName(*name, cfg)
	if flow.Name != "" {
		defaultName = flow.Name
	}
	defaultCWD := *cwd
	if flow.CWD != "" {
		defaultCWD = flow.CWD
	}
	manager := newManager(*stateDir, defaultCWD)
	ctx := context.Background()
	var results []*acp.RuntimeTurnResult
	for _, step := range flow.Steps {
		stepAgent := defaultAgent
		if step.Agent != "" {
			stepAgent = step.Agent
		}
		stepCWD := defaultCWD
		if step.CWD != "" {
			stepCWD = step.CWD
		}
		stepName := defaultName
		if step.Name != "" {
			stepName = step.Name
		}
		promptText, err := readPromptInput(nil, step.Prompt, step.File)
		if err != nil {
			return err
		}
		if step.Mode == "exec" {
			result, err := manager.RunOnceWithOptions(ctx, stepAgent, stepCWD, promptText, sessionOptions, nil)
			if err != nil {
				return err
			}
			results = append(results, result)
			continue
		}
		handle, err := manager.EnsureSession(ctx, acp.RuntimeEnsureInput{
			Agent:          stepAgent,
			CWD:            stepCWD,
			Name:           stepName,
			Mode:           acp.RuntimeSessionModePersistent,
			SessionOptions: sessionOptions,
		})
		if err != nil {
			return err
		}
		result, err := manager.RunTurn(ctx, acp.RuntimeTurnInput{
			Handle: *handle,
			Text:   promptText,
		}, nil)
		if err != nil {
			return err
		}
		results = append(results, result)
	}
	if *jsonOutput {
		return printJSON(stdout, results)
	}
	for _, result := range results {
		if strings.TrimSpace(result.Text) == "" {
			continue
		}
		_, _ = fmt.Fprintln(stdout, result.Text)
	}
	return nil
}

func ensureOwnerProcess(stateDir, recordID string) error {
	queue := acp.NewFileQueueStore(stateDir)
	health, err := queue.ProbeOwner(recordID)
	if err != nil {
		return err
	}
	if health != nil && health.Healthy {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "internal-owner", "--state-dir", stateDir, "--record-id", recordID)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}

func runInternalOwner(args []string) error {
	fs := flag.NewFlagSet("internal-owner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", acp.DefaultStateDir(), "")
	recordID := fs.String("record-id", "", "")
	idleTimeout := fs.Duration("idle-timeout", 2*time.Minute, "")
	pollInterval := fs.Duration("poll-interval", 500*time.Millisecond, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*recordID) == "" {
		return fmt.Errorf("record-id is required")
	}
	manager := newManager(*stateDir, "")
	queue := acp.NewFileQueueStore(*stateDir)
	wakeCh := make(chan struct{}, 1)
	acquired, err := queue.TryAcquireLease(&acp.QueueOwnerLease{
		RecordID:    *recordID,
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(),
		QueueDepth:  0,
		SocketPath:  ownerIPCSocketPath(*stateDir, *recordID),
	})
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	ipcServer := newOwnerIPCServer(*stateDir, *recordID, queueLeaseGeneration(queue, *recordID), queue, manager, wakeCh)
	if err := ipcServer.Start(); err != nil {
		_ = queue.ClearLease(*recordID)
		return err
	}
	resetRequests, err := queue.RequeueOrphanedRunningRequests(*recordID)
	if err != nil {
		_ = ipcServer.Close()
		return err
	}
	if len(resetRequests) > 0 {
		record, loadErr := manager.LoadRecord(*recordID)
		if loadErr == nil && record != nil {
			record.ActiveRequestID = ""
			record.OwnerPID = 0
			_ = acp.NewFileSessionStore(*stateDir).Save(record)
		}
	}
	defer func() {
		_ = ipcServer.Close()
		_ = queue.ClearLease(*recordID)
		record, err := manager.LoadRecord(*recordID)
		if err == nil && record != nil {
			record.ActiveRequestID = ""
			record.OwnerPID = 0
			_ = acp.NewFileSessionStore(*stateDir).Save(record)
		}
	}()
	heartbeatTicker := time.NewTicker(acp.DefaultQueueOwnerHeartbeatInterval())
	defer heartbeatTicker.Stop()
	lastWork := time.Now()
	for {
		if ipcServer.ShouldClose() {
			if _, err := manager.CloseSession(context.Background(), *recordID); err != nil {
				return err
			}
			return nil
		}
		select {
		case <-heartbeatTicker.C:
			pending, _ := queue.List(*recordID)
			queueDepth := 0
			for _, item := range pending {
				if item.Status == acp.QueueRequestQueued || item.Status == acp.QueueRequestRunning {
					queueDepth++
				}
			}
			_, _ = queue.RefreshLease(*recordID, os.Getpid(), queueDepth)
		default:
		}
		request, err := queue.NextPending(*recordID)
		if err != nil {
			return err
		}
		if request == nil {
			if time.Since(lastWork) >= *idleTimeout {
				return nil
			}
			timer := time.NewTimer(*pollInterval)
			select {
			case <-timer.C:
			case <-wakeCh:
				if !timer.Stop() {
					<-timer.C
				}
			}
			continue
		}
		lastWork = time.Now()
		record, err := manager.LoadRecord(*recordID)
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("session record %q not found", *recordID)
		}
		now := time.Now().UTC()
		request.Status = acp.QueueRequestRunning
		request.StartedAt = &now
		request.Error = ""
		if err := queue.Save(request); err != nil {
			return err
		}
		_, _ = queue.RefreshLease(*recordID, os.Getpid(), 1)
		record.ActiveRequestID = request.RequestID
		record.OwnerPID = os.Getpid()
		if err := acp.NewFileSessionStore(*stateDir).Save(record); err != nil {
			return err
		}
		var runErr error
		switch request.Kind {
		case "", acp.QueueRequestPrompt:
			cancelCh := make(chan struct{})
			var cancelOnce sync.Once
			signalCancel := func() {
				cancelOnce.Do(func() {
					close(cancelCh)
				})
			}
			cancelDone := make(chan struct{})
			go func(requestID string) {
				defer close(cancelDone)
				ticker := time.NewTicker(*pollInterval)
				defer ticker.Stop()
				for {
					select {
					case <-cancelCh:
						return
					case <-ticker.C:
						latest, err := queue.Load(*recordID, requestID)
						if err == nil && latest != nil && latest.CancelRequested {
							signalCancel()
							return
						}
					}
				}
			}(request.RequestID)
			ipcServer.RegisterPromptCanceler(request.RequestID, signalCancel)
			var rawMu sync.Mutex
			result, err := manager.RunTurnWithCancelObserved(context.Background(), acp.RuntimeTurnInput{
				Handle: acp.RuntimeHandle{
					RecordID:         record.RecordID,
					SessionKey:       record.SessionKey,
					Agent:            record.Agent,
					CWD:              record.CWD,
					Name:             record.Name,
					BackendSessionID: record.BackendSessionID,
				},
				Text: request.Prompt,
			}, cancelCh, acp.RuntimeTurnCallbacks{
				OnEvent: func(event acp.RuntimeEvent) {
					ipcServer.EmitEvent(request.RequestID, event)
				},
				OnRawMessage: func(message json.RawMessage) {
					rawMu.Lock()
					defer rawMu.Unlock()
					_ = queue.AppendRawMessage(*recordID, request.RequestID, message)
					ipcServer.EmitRawMessage(request.RequestID, message)
				},
				OnActiveController: func(controller acp.RuntimeActiveSessionController) {
					ipcServer.RegisterActiveController(controller)
				},
				OnActiveControllerClosed: func() {
					ipcServer.UnregisterActiveController()
				},
			})
			signalCancel()
			ipcServer.UnregisterPromptCanceler(request.RequestID)
			<-cancelDone
			if err != nil {
				runErr = err
			} else {
				request.ResultText = result.Text
				request.StopReason = string(result.StopReason)
				if result.StopReason == acp.StopReasonCancelled || request.CancelRequested {
					request.Status = acp.QueueRequestCancelled
				} else {
					request.Status = acp.QueueRequestCompleted
				}
			}
		case acp.QueueRequestSetMode:
			runErr = manager.SetSessionMode(context.Background(), record.RecordID, acp.SessionModeId(request.ModeID))
			if runErr == nil {
				request.Status = acp.QueueRequestCompleted
			}
		case acp.QueueRequestSetConfig:
			runErr = manager.SetSessionConfigOption(context.Background(), record.RecordID, acp.SessionConfigId(request.ConfigID), acp.SessionConfigValueId(request.ConfigValue))
			if runErr == nil {
				request.Status = acp.QueueRequestCompleted
			}
		case acp.QueueRequestSetModel:
			runErr = manager.SetSessionModel(context.Background(), record.RecordID, request.ModelID)
			if runErr == nil {
				request.Status = acp.QueueRequestCompleted
			}
		case acp.QueueRequestClose:
			_, runErr = manager.CloseSession(context.Background(), record.RecordID)
			if runErr == nil {
				request.Status = acp.QueueRequestCompleted
			}
		default:
			runErr = fmt.Errorf("unsupported queue request kind %q", request.Kind)
		}
		finished := time.Now().UTC()
		request.FinishedAt = &finished
		if runErr != nil {
			request.Status = acp.QueueRequestFailed
			request.Error = runErr.Error()
		}
		if err := queue.Save(request); err != nil {
			return err
		}
		if request.Status == acp.QueueRequestFailed {
			ipcServer.EmitError(request.RequestID, fmt.Errorf("%s", request.Error))
		} else if request.Kind == acp.QueueRequestPrompt || request.Kind == "" {
			ipcServer.EmitResult(request.RequestID, request)
		}
		pending, _ := queue.List(*recordID)
		queueDepth := 0
		for _, item := range pending {
			if item.Status == acp.QueueRequestQueued || item.Status == acp.QueueRequestRunning {
				queueDepth++
			}
		}
		_, _ = queue.RefreshLease(*recordID, os.Getpid(), queueDepth)
		record, err = manager.LoadRecord(*recordID)
		if err != nil {
			return err
		}
		if record != nil {
			record.ActiveRequestID = ""
			record.OwnerPID = os.Getpid()
			if err := acp.NewFileSessionStore(*stateDir).Save(record); err != nil {
				return err
			}
		}
		if request.Kind == acp.QueueRequestClose && request.Status == acp.QueueRequestCompleted {
			return nil
		}
		if ipcServer.ShouldClose() {
			if _, err := manager.CloseSession(context.Background(), *recordID); err != nil {
				return err
			}
			return nil
		}
	}
}
