package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/topcheer/ggcode-acp-go"
)

func TestSessionShouldUseQueueForPendingRequest(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-1"
	if _, err := queue.Enqueue(recordID, "hello"); err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	if !sessionShouldUseQueue(queue, recordID) {
		t.Fatalf("expected pending request to force queue routing")
	}
}

func TestWaitForQueueRequestReturnsTerminalRecord(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-1"
	request, err := queue.Enqueue(recordID, "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		now := time.Now().UTC()
		request.Status = acp.QueueRequestCompleted
		request.FinishedAt = &now
		request.ResultText = "done"
		_ = queue.Save(request)
	}()
	completed, err := waitForQueueRequest(queue, recordID, request.RequestID, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForQueueRequest error: %v", err)
	}
	<-done
	if completed.Status != acp.QueueRequestCompleted || completed.ResultText != "done" {
		t.Fatalf("unexpected completed request: %#v", completed)
	}
}

func TestSessionShouldUseQueueForHealthyOwner(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-healthy"
	now := time.Now().UTC()
	if err := queue.SaveLease(&acp.QueueOwnerLease{
		RecordID:    recordID,
		PID:         os.Getpid(),
		StartedAt:   now,
		HeartbeatAt: now,
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	if !sessionShouldUseQueue(queue, recordID) {
		t.Fatalf("expected healthy owner to force queue routing")
	}
}

func TestWaitForQueuedPromptResultStreamsHistoryEvents(t *testing.T) {
	root := t.TempDir()
	manager := newManager(root, "")
	queue := acp.NewFileQueueStore(root)
	history := acp.NewFileHistoryStore(root)
	recordID := "record-stream"
	request, err := queue.Enqueue(recordID, "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(20 * time.Millisecond)
		started := time.Now().UTC()
		request.Status = acp.QueueRequestRunning
		request.StartedAt = &started
		_ = queue.Save(request)
		_ = history.Append(recordID, acp.SessionHistoryEntry{
			Timestamp: started.Add(10 * time.Millisecond),
			Kind:      "text",
			Text:      "partial",
		})
		time.Sleep(20 * time.Millisecond)
		finished := time.Now().UTC()
		request.Status = acp.QueueRequestCompleted
		request.FinishedAt = &finished
		request.ResultText = "partial"
		_ = queue.Save(request)
	}()
	var events []acp.RuntimeEvent
	completed, err := waitForQueuedPromptResult(manager, queue, recordID, request.RequestID, 10*time.Millisecond, func(event acp.RuntimeEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("waitForQueuedPromptResult error: %v", err)
	}
	<-done
	if completed.Status != acp.QueueRequestCompleted {
		t.Fatalf("unexpected status: %#v", completed)
	}
	if len(events) != 1 || events[0].Type != acp.RuntimeEventTextDelta || events[0].Text != "partial" {
		t.Fatalf("unexpected streamed events: %#v", events)
	}
}

func TestWaitForQueuedPromptJSONStreamsRawMessages(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-json"
	request, err := queue.Enqueue(recordID, "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(20 * time.Millisecond)
		started := time.Now().UTC()
		request.Status = acp.QueueRequestRunning
		request.StartedAt = &started
		_ = queue.Save(request)
		_ = queue.AppendRawMessage(recordID, request.RequestID, []byte(`{"jsonrpc":"2.0","method":"session/prompt"}`))
		time.Sleep(20 * time.Millisecond)
		_ = queue.AppendRawMessage(recordID, request.RequestID, []byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`))
		finished := time.Now().UTC()
		request.Status = acp.QueueRequestCompleted
		request.FinishedAt = &finished
		_ = queue.Save(request)
	}()
	var output bytes.Buffer
	completed, err := waitForQueuedPromptJSON(queue, recordID, request.RequestID, 10*time.Millisecond, &output)
	if err != nil {
		t.Fatalf("waitForQueuedPromptJSON error: %v", err)
	}
	<-done
	if completed.Status != acp.QueueRequestCompleted {
		t.Fatalf("unexpected status: %#v", completed)
	}
	got := output.String()
	if !containsAll(got, []string{`{"jsonrpc":"2.0","method":"session/prompt"}`, `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`}) {
		t.Fatalf("unexpected raw json output:\n%s", got)
	}
}

func TestStreamQueuedPromptViaOwnerSocketStreamsPromptEvents(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-socket"
	socketDir, err := os.MkdirTemp("", "acpx-socket-*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "owner.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatalf("MkdirAll error: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	now := time.Now().UTC()
	if err := queue.SaveLease(&acp.QueueOwnerLease{
		RecordID:        recordID,
		PID:             os.Getpid(),
		StartedAt:       now,
		HeartbeatAt:     now,
		SocketPath:      socketPath,
		OwnerGeneration: 7,
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept error: %v", err)
			return
		}
		defer conn.Close()
		var request ownerIPCSubmitPromptRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			t.Errorf("Decode error: %v", err)
			return
		}
		if request.Type != "submit_prompt" || request.Prompt != "hello" || !request.Wait {
			t.Errorf("unexpected request: %#v", request)
			return
		}
		encoder := json.NewEncoder(conn)
		_ = encoder.Encode(ownerIPCEnvelope{Type: "accepted", RequestID: "req-1", OwnerGeneration: 7})
		_ = encoder.Encode(ownerIPCEnvelope{
			Type:            "event",
			RequestID:       "req-1",
			OwnerGeneration: 7,
			Event:           &acp.RuntimeEvent{Type: acp.RuntimeEventTextDelta, Text: "partial"},
		})
		_ = encoder.Encode(ownerIPCEnvelope{
			Type:            "result",
			RequestID:       "req-1",
			OwnerGeneration: 7,
			Result: &ownerIPCResult{
				Status: "completed",
				Text:   "partial",
			},
		})
	}()
	var events []acp.RuntimeEvent
	result, handled, err := streamQueuedPromptViaOwnerSocket(root, recordID, "hello", false, &bytes.Buffer{}, func(event acp.RuntimeEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("streamQueuedPromptViaOwnerSocket error: %v", err)
	}
	<-done
	if !handled {
		t.Fatalf("expected socket path to be handled")
	}
	if result == nil || result.RequestID != "req-1" || result.Text != "partial" || result.Status != "completed" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(events) != 1 || events[0].Type != acp.RuntimeEventTextDelta || events[0].Text != "partial" {
		t.Fatalf("unexpected streamed events: %#v", events)
	}
}

func TestRequestOwnerSocketCancelReturnsLiveResult(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-cancel"
	socketDir, err := os.MkdirTemp("", "acpx-socket-*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "owner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	now := time.Now().UTC()
	if err := queue.SaveLease(&acp.QueueOwnerLease{
		RecordID:        recordID,
		PID:             os.Getpid(),
		StartedAt:       now,
		HeartbeatAt:     now,
		SocketPath:      socketPath,
		OwnerGeneration: 11,
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept error: %v", err)
			return
		}
		defer conn.Close()
		var request ownerIPCSubmitPromptRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			t.Errorf("Decode error: %v", err)
			return
		}
		if request.Type != "cancel_prompt" || request.RequestID != "req-2" {
			t.Errorf("unexpected cancel request: %#v", request)
			return
		}
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "cancel_result",
			RequestID:       "req-2",
			OwnerGeneration: 11,
			Cancelled:       true,
		})
	}()
	handled, cancelled, err := requestOwnerSocketCancel(root, recordID, "req-2")
	if err != nil {
		t.Fatalf("requestOwnerSocketCancel error: %v", err)
	}
	<-done
	if !handled || !cancelled {
		t.Fatalf("unexpected cancel result handled=%t cancelled=%t", handled, cancelled)
	}
}

func TestRequestOwnerSocketSetModeReturnsLiveResult(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-set-mode"
	socketDir, err := os.MkdirTemp("", "acpx-socket-*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "owner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	now := time.Now().UTC()
	if err := queue.SaveLease(&acp.QueueOwnerLease{
		RecordID:        recordID,
		PID:             os.Getpid(),
		StartedAt:       now,
		HeartbeatAt:     now,
		SocketPath:      socketPath,
		OwnerGeneration: 13,
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept error: %v", err)
			return
		}
		defer conn.Close()
		var request ownerIPCSubmitPromptRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			t.Errorf("Decode error: %v", err)
			return
		}
		if request.Type != "set_mode" || request.ModeID != "plan" {
			t.Errorf("unexpected set_mode request: %#v", request)
			return
		}
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "set_mode_result",
			OwnerGeneration: 13,
			ModeID:          "plan",
		})
	}()
	handled, err := requestOwnerSocketSetMode(root, recordID, "plan")
	if err != nil {
		t.Fatalf("requestOwnerSocketSetMode error: %v", err)
	}
	<-done
	if !handled {
		t.Fatalf("expected set-mode socket request to be handled")
	}
}

func TestRequestOwnerSocketCloseReturnsLiveResult(t *testing.T) {
	root := t.TempDir()
	queue := acp.NewFileQueueStore(root)
	recordID := "record-close"
	socketDir, err := os.MkdirTemp("", "acpx-socket-*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "owner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	now := time.Now().UTC()
	if err := queue.SaveLease(&acp.QueueOwnerLease{
		RecordID:        recordID,
		PID:             os.Getpid(),
		StartedAt:       now,
		HeartbeatAt:     now,
		SocketPath:      socketPath,
		OwnerGeneration: 17,
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept error: %v", err)
			return
		}
		defer conn.Close()
		var request ownerIPCSubmitPromptRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			t.Errorf("Decode error: %v", err)
			return
		}
		if request.Type != "close_session" {
			t.Errorf("unexpected close_session request: %#v", request)
			return
		}
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "close_session_result",
			OwnerGeneration: 17,
			Closed:          true,
		})
	}()
	handled, closed, err := requestOwnerSocketClose(root, recordID)
	if err != nil {
		t.Fatalf("requestOwnerSocketClose error: %v", err)
	}
	<-done
	if !handled || !closed {
		t.Fatalf("unexpected close result handled=%t closed=%t", handled, closed)
	}
}

func TestHistoryEntryToRuntimeEventPreservesToolMetadata(t *testing.T) {
	event, ok := historyEntryToRuntimeEvent(acp.SessionHistoryEntry{
		Kind:      "tool_call",
		ToolID:    "call-1",
		ToolName:  "bash",
		ToolTitle: "Run Bash",
		ToolArgs:  "{\"command\":\"echo hi\"}",
	})
	if !ok {
		t.Fatalf("expected tool_call to convert")
	}
	if event.ToolID != "call-1" || event.ToolTitle != "Run Bash" || event.ToolArgs != "{\"command\":\"echo hi\"}" {
		t.Fatalf("unexpected runtime event: %#v", event)
	}
}

func TestHistoryEntryToRuntimeEventPreservesPermissionEscalation(t *testing.T) {
	event, ok := historyEntryToRuntimeEvent(acp.SessionHistoryEntry{
		Kind: "permission_escalation",
		Metadata: json.RawMessage(`{
			"type":"permission_escalation",
			"sessionId":"session-1",
			"toolCallId":"call-2",
			"toolTitle":"Run build",
			"toolInput":{"command":"make test"},
			"message":"Approval required for Run build",
			"timestamp":"2026-06-02T00:00:00Z",
			"action":"escalate"
		}`),
	})
	if !ok {
		t.Fatalf("expected permission escalation to convert")
	}
	if event.Type != acp.RuntimeEventPermissionEscalation || event.PermissionEscalation == nil {
		t.Fatalf("unexpected runtime event: %#v", event)
	}
	if event.PermissionEscalation.ToolCallID != "call-2" || event.PermissionEscalation.ToolTitle != "Run build" {
		t.Fatalf("unexpected escalation payload: %#v", event.PermissionEscalation)
	}
}

func TestPromptRendererRendersToolLifecycle(t *testing.T) {
	var output bytes.Buffer
	renderer := newPromptRenderer(&output)
	renderer.render(acp.RuntimeEvent{Type: acp.RuntimeEventTextDelta, Text: "Hello"})
	renderer.render(acp.RuntimeEvent{
		Type:      acp.RuntimeEventToolCall,
		ToolTitle: "Run Bash",
		ToolArgs:  "{\"command\":\"echo hi\"}",
	})
	renderer.render(acp.RuntimeEvent{
		Type:      acp.RuntimeEventToolResult,
		ToolTitle: "Run Bash",
		ToolArgs:  "{\"command\":\"echo hi\"}",
		Text:      "hi",
	})
	renderer.finish()

	got := output.String()
	if !containsAll(got, []string{"Hello", "[tool] Run Bash (running)", "input:", "\"command\":\"echo hi\"", "[tool] Run Bash (completed)", "output:", "hi"}) {
		t.Fatalf("unexpected renderer output:\n%s", got)
	}
}

func TestPromptRendererRendersPermissionEscalation(t *testing.T) {
	var output bytes.Buffer
	renderer := newPromptRenderer(&output)
	renderer.render(acp.RuntimeEvent{
		Type: acp.RuntimeEventPermissionEscalation,
		PermissionEscalation: &acp.PermissionEscalationEvent{
			Type:       "permission_escalation",
			SessionID:  "session-1",
			ToolCallID: "call-3",
			ToolTitle:  "Run build",
			ToolKind:   acp.ToolKindExecute,
			ToolInput:  json.RawMessage(`{"command":"make test"}`),
			Action:     "escalate",
			Message:    "Approval required for Run build",
			Timestamp:  "2026-06-02T00:00:00Z",
		},
	})
	renderer.finish()
	got := output.String()
	if !containsAll(got, []string{"[permission] Approval required for Run build", "sessionId: session-1", "toolCallId: call-3", "toolTitle: Run build", "toolInput: {\"command\":\"make test\"}", "toolKind: execute"}) {
		t.Fatalf("unexpected permission output:\n%s", got)
	}
}

func TestFinalizePromptOutputKeepsFinalAssistantTextAfterTools(t *testing.T) {
	var output bytes.Buffer
	renderer := newPromptRenderer(&output)
	renderer.render(acp.RuntimeEvent{
		Type:      acp.RuntimeEventToolCall,
		ToolTitle: "Run Bash",
		ToolArgs:  "{\"command\":\"echo hi\"}",
	})
	renderer.render(acp.RuntimeEvent{
		Type:      acp.RuntimeEventToolResult,
		ToolTitle: "Run Bash",
		Text:      "hi",
	})
	finalizePromptOutput(renderer, "Final answer")

	got := output.String()
	if !containsAll(got, []string{"[tool] Run Bash (running)", "[tool] Run Bash (completed)", "Final answer"}) {
		t.Fatalf("unexpected final prompt output:\n%s", got)
	}
}

func TestPersistQueuedControlStateUpdatesModeAndConfig(t *testing.T) {
	root := t.TempDir()
	store := acp.NewFileSessionStore(root)
	record := &acp.SessionRecord{
		RecordID:     "record-1",
		Name:         "demo",
		CWD:          filepath.Clean(root),
		Agent:        "copilot",
		HistoryPath:  filepath.Join(root, "history", "record-1.ndjson"),
		ConfigValues: map[string]string{"temperature": "low"},
	}
	if err := store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	if err := persistQueuedControlState(root, record.RecordID, "plan", "", "", "", false); err != nil {
		t.Fatalf("persist mode error: %v", err)
	}
	if err := persistQueuedControlState(root, record.RecordID, "", "gpt-5.4", "", "", false); err != nil {
		t.Fatalf("persist model error: %v", err)
	}
	if err := persistQueuedControlState(root, record.RecordID, "", "", "temperature", "high", false); err != nil {
		t.Fatalf("persist config error: %v", err)
	}
	updated, err := store.Load(record.RecordID)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if updated.Mode != "plan" {
		t.Fatalf("expected mode to be updated, got %q", updated.Mode)
	}
	if updated.ConfigValues["model"] != "gpt-5.4" {
		t.Fatalf("expected model to be updated, got %#v", updated.ConfigValues)
	}
	if updated.ConfigValues["temperature"] != "high" {
		t.Fatalf("expected config value to be updated, got %#v", updated.ConfigValues)
	}
}

func TestSessionHasActiveOwnerPrompt(t *testing.T) {
	if sessionHasActiveOwnerPrompt(&acp.RuntimeStatus{OwnerAlive: true, ActiveRequestID: "req-1"}) != true {
		t.Fatalf("expected active owner prompt")
	}
	if sessionHasActiveOwnerPrompt(&acp.RuntimeStatus{OwnerAlive: true}) {
		t.Fatalf("expected idle owner to be false")
	}
	if sessionHasActiveOwnerPrompt(&acp.RuntimeStatus{ActiveRequestID: "req-1"}) {
		t.Fatalf("expected missing owner liveness to be false")
	}
}

func TestRunPromptJSONPrintsJSONRPCError(t *testing.T) {
	var output bytes.Buffer
	err := runPrompt([]string{"--json", "--state-dir", t.TempDir()}, "", &output)
	if err != nil {
		t.Fatalf("expected runPrompt to emit json error instead of returning one, got %v", err)
	}
	got := output.String()
	if !containsAll(got, []string{`"jsonrpc":"2.0"`, `"code":-32603`, `"acpxCode":"RUNTIME"`}) {
		t.Fatalf("unexpected json error output:\n%s", got)
	}
}

func containsAll(text string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
