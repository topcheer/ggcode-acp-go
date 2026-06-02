package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionUpdateUnmarshalTracksToolFieldPresence(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{
		"sessionUpdate":"tool_call_update",
		"toolCallId":"call-1",
		"rawInput":{},
		"rawOutput":"denied"
	}`), &update); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if !update.hasToolCall || !update.hasRawInput || !update.hasRawOutput {
		t.Fatalf("expected tool_call_update presence flags to be tracked: %#v", update)
	}
	if update.hasTitle || update.hasStatus || update.hasLocations {
		t.Fatalf("unexpected presence flags set: %#v", update)
	}
}

func TestHandleSessionUpdateMergesToolMetadataAcrossUpdates(t *testing.T) {
	client := &Client{}
	client.promptToolMeta = make(map[string]*promptToolState)
	client.activePromptID = "session-1"
	client.promptDone = make(chan PromptResponse, 1)
	client.promptActivity = make(chan struct{}, 1)

	var events []PromptEvent
	client.promptOnEvent = func(event PromptEvent) {
		events = append(events, event)
	}

	updates := []string{
		`{
			"sessionId":"session-1",
			"update":{
				"sessionUpdate":"tool_call",
				"toolCallId":"call-1",
				"title":"Write",
				"kind":"edit",
				"status":"pending",
				"rawInput":{}
			}
		}`,
		`{
			"sessionId":"session-1",
			"update":{
				"sessionUpdate":"tool_call_update",
				"toolCallId":"call-1",
				"title":"Write /tmp/hello.txt",
				"kind":"edit",
				"rawInput":{"file_path":"/tmp/hello.txt","content":"hello from test\n"},
				"content":[{"type":"diff","path":"/tmp/hello.txt","newText":"hello from test\n"}]
			}
		}`,
		`{
			"sessionId":"session-1",
			"update":{
				"sessionUpdate":"tool_call_update",
				"toolCallId":"call-1",
				"status":"failed",
				"rawOutput":"permission denied"
			}
		}`,
	}

	for _, payload := range updates {
		client.handleSessionUpdate(&JSONRPCRequest{Params: json.RawMessage(payload)})
	}

	if len(events) != 2 {
		t.Fatalf("expected start and terminal events, got %#v", events)
	}
	start := events[0]
	if start.Type != PromptEventToolCall || start.ToolTitle != "Write" {
		t.Fatalf("unexpected start event: %#v", start)
	}
	if start.ToolArgs != "" {
		t.Fatalf("expected empty start args for empty-object rawInput, got %#v", start)
	}
	result := events[1]
	if result.Type != PromptEventToolResult {
		t.Fatalf("unexpected terminal event: %#v", result)
	}
	if result.ToolName != "Write" {
		t.Fatalf("expected stable tool name from initial call, got %#v", result)
	}
	if result.ToolTitle != "Write /tmp/hello.txt" {
		t.Fatalf("expected merged title on result, got %#v", result)
	}
	if !strings.Contains(result.ToolArgs, `"file_path":"/tmp/hello.txt"`) {
		t.Fatalf("expected merged tool args on result, got %#v", result)
	}
	if result.Result != `"permission denied"` || !result.IsError {
		t.Fatalf("expected merged terminal result, got %#v", result)
	}
	if len(client.promptTools) != 1 {
		t.Fatalf("expected one tool summary, got %#v", client.promptTools)
	}
	if client.promptTools[0].Title != "Write /tmp/hello.txt" || client.promptTools[0].Status != "failed" {
		t.Fatalf("expected merged tool summary, got %#v", client.promptTools[0])
	}
}
