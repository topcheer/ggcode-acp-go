package acp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestPermissionRequestResponseUsesStableToolKeyAndPersistentAllow(t *testing.T) {
	policy := NewConfigPolicyWithMode(map[string]Decision{}, nil, SupervisedMode)
	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, t.TempDir(), policy, nil)

	var gotToolName string
	var gotInput string
	client.SetApprovalHandler(func(_ context.Context, toolName string, input string) Decision {
		gotToolName = toolName
		gotInput = input
		policy.SetOverride(toolName, Allow)
		return Allow
	})

	rawInput := json.RawMessage(`{"command":"make test"}`)
	resp, err := client.permissionRequestResponse(context.Background(), RequestPermissionRequest{
		ToolCall: &ToolCallUpdate{
			Kind:     ToolKindExecute,
			Title:    "Run build",
			RawInput: rawInput,
		},
		Options: []PermissionOption{
			{OptionID: "allow-once", Kind: PermissionOptionAllowOnce},
			{OptionID: "allow-always", Kind: PermissionOptionAllowAlways},
			{OptionID: "reject-once", Kind: PermissionOptionRejectOnce},
		},
	}, rawInput)
	if err != nil {
		t.Fatalf("permissionRequestResponse error: %v", err)
	}
	if gotToolName != "delegate_copilot_execute" {
		t.Fatalf("expected stable tool name, got %q", gotToolName)
	}
	if gotInput != string(rawInput) {
		t.Fatalf("expected raw input to be forwarded, got %q", gotInput)
	}
	if resp.Outcome.Outcome != "selected" || resp.Outcome.SelectedOption == nil || resp.Outcome.SelectedOption.OptionID != "allow-always" {
		t.Fatalf("expected persistent allow response, got %+v", resp.Outcome)
	}
}

func TestPermissionRequestResponseSkipsApprovalWhenPolicyAlreadyAllows(t *testing.T) {
	policy := NewConfigPolicyWithMode(map[string]Decision{}, nil, SupervisedMode)
	policy.SetOverride("delegate_copilot_execute", Allow)

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, t.TempDir(), policy, nil)
	approvalCalls := 0
	client.SetApprovalHandler(func(_ context.Context, toolName string, input string) Decision {
		approvalCalls++
		return Allow
	})

	rawInput := json.RawMessage(`{"command":"go test ./..."}`)
	resp, err := client.permissionRequestResponse(context.Background(), RequestPermissionRequest{
		ToolCall: &ToolCallUpdate{
			Kind:     ToolKindExecute,
			RawInput: rawInput,
		},
		Options: []PermissionOption{
			{OptionID: "allow-always", Kind: PermissionOptionAllowAlways},
			{OptionID: "allow-once", Kind: PermissionOptionAllowOnce},
			{OptionID: "reject-once", Kind: PermissionOptionRejectOnce},
		},
	}, rawInput)
	if err != nil {
		t.Fatalf("permissionRequestResponse error: %v", err)
	}
	if approvalCalls != 0 {
		t.Fatalf("expected policy allow to bypass approval, got %d approval calls", approvalCalls)
	}
	if resp.Outcome.Outcome != "selected" || resp.Outcome.SelectedOption == nil || resp.Outcome.SelectedOption.OptionID != "allow-always" {
		t.Fatalf("expected policy allow to select persistent allow option, got %+v", resp.Outcome)
	}
}

func TestPermissionRequestResponseHonorsBypassModeWithoutApproval(t *testing.T) {
	policy := NewConfigPolicyWithMode(map[string]Decision{}, nil, BypassMode)
	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, t.TempDir(), policy, nil)

	approvalCalls := 0
	client.SetApprovalHandler(func(_ context.Context, toolName string, input string) Decision {
		approvalCalls++
		return Deny
	})

	rawInput := json.RawMessage(`{"command":"git --no-pager status --short"}`)
	resp, err := client.permissionRequestResponse(context.Background(), RequestPermissionRequest{
		ToolCall: &ToolCallUpdate{
			Kind:     ToolKindExecute,
			Title:    "Show status",
			RawInput: rawInput,
		},
		Options: []PermissionOption{
			{OptionID: "allow-always", Kind: PermissionOptionAllowAlways},
			{OptionID: "allow-once", Kind: PermissionOptionAllowOnce},
			{OptionID: "reject-once", Kind: PermissionOptionRejectOnce},
		},
	}, rawInput)
	if err != nil {
		t.Fatalf("permissionRequestResponse error: %v", err)
	}
	if approvalCalls != 0 {
		t.Fatalf("expected bypass mode to skip approval, got %d approval calls", approvalCalls)
	}
	if resp.Outcome.Outcome != "selected" || resp.Outcome.SelectedOption == nil || resp.Outcome.SelectedOption.OptionID != "allow-always" {
		t.Fatalf("expected bypass mode to select allow option, got %+v", resp.Outcome)
	}
}

func TestRequestPermissionRequestUnmarshalAcceptsObjectRawInput(t *testing.T) {
	var req RequestPermissionRequest
	err := json.Unmarshal([]byte(`{
		"sessionId":"session-1",
		"toolCall":{
			"title":"Show status",
			"kind":"execute",
			"rawInput":{"command":"git --no-pager status --short"}
		},
		"options":[{"optionId":"allow-once","kind":"allow_once"}]
	}`), &req)
	if err != nil {
		t.Fatalf("unmarshal permission request: %v", err)
	}
	if req.ToolCall == nil {
		t.Fatal("expected toolCall")
	}
	if string(req.ToolCall.RawInput) != `{"command":"git --no-pager status --short"}` {
		t.Fatalf("expected rawInput object payload, got %s", string(req.ToolCall.RawInput))
	}
}

func TestRequestPermissionRequestUnmarshalAcceptsToolCallContentArray(t *testing.T) {
	var req RequestPermissionRequest
	err := json.Unmarshal([]byte(`{
		"sessionId":"session-1",
		"toolCall":{
			"toolCallId":"call-1",
			"title":"Writing hello.txt",
			"kind":"edit",
			"status":"pending",
			"content":[
				{
					"type":"diff",
					"path":"/tmp/hello.txt",
					"oldText":"",
					"newText":"hello"
				}
			],
			"rawInput":{"file_path":"/tmp/hello.txt","content":"hello"}
		},
		"options":[{"optionId":"allow-once","kind":"allow_once"}]
	}`), &req)
	if err != nil {
		t.Fatalf("unmarshal permission request with content array: %v", err)
	}
	if req.ToolCall == nil || req.ToolCall.Content == nil {
		t.Fatal("expected toolCall content")
	}
	if req.ToolCall.Content.Type != "diff" {
		t.Fatalf("expected diff content, got %+v", req.ToolCall.Content)
	}
	if req.ToolCall.Content.Diff == nil || req.ToolCall.Content.Diff.Path != "/tmp/hello.txt" {
		t.Fatalf("expected diff path to be preserved, got %+v", req.ToolCall.Content.Diff)
	}
}

func TestPermissionRequestResponseFallsBackToAllowOptionIDWithoutKind(t *testing.T) {
	policy := NewConfigPolicyWithMode(map[string]Decision{}, nil, BypassMode)
	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, t.TempDir(), policy, nil)

	rawInput := json.RawMessage(`{"command":"git --no-pager status --short"}`)
	resp, err := client.permissionRequestResponse(context.Background(), RequestPermissionRequest{
		ToolCall: &ToolCallUpdate{
			Kind:     ToolKindExecute,
			Title:    "Show status",
			RawInput: rawInput,
		},
		Options: []PermissionOption{
			{OptionID: "allow"},
			{OptionID: "reject"},
		},
	}, rawInput)
	if err != nil {
		t.Fatalf("permissionRequestResponse error: %v", err)
	}
	if resp.Outcome.Outcome != "selected" || resp.Outcome.SelectedOption == nil || resp.Outcome.SelectedOption.OptionID != "allow" {
		t.Fatalf("expected optionId fallback to select allow, got %+v", resp.Outcome)
	}
}

func TestPermissionRequestResponseEmitsEscalationWhenApprovalIsNeeded(t *testing.T) {
	policy := NewConfigPolicyWithMode(map[string]Decision{}, nil, SupervisedMode)
	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, t.TempDir(), policy, nil)
	rawInput := json.RawMessage(`{"command":"make test"}`)
	var event PermissionEscalationEvent
	client.SetPermissionEscalationHandler(func(value PermissionEscalationEvent) {
		event = value
	})
	client.SetApprovalHandler(func(_ context.Context, toolName string, input string) Decision {
		return Allow
	})

	_, err := client.permissionRequestResponse(context.Background(), RequestPermissionRequest{
		SessionID: "session-1",
		ToolCall: &ToolCallUpdate{
			ToolCallID: "call-1",
			Kind:       ToolKindExecute,
			Title:      "Run build",
			RawInput:   rawInput,
		},
		Options: []PermissionOption{
			{OptionID: "allow-once", Kind: PermissionOptionAllowOnce},
			{OptionID: "allow-always", Kind: PermissionOptionAllowAlways},
		},
	}, rawInput)
	if err != nil {
		t.Fatalf("permissionRequestResponse error: %v", err)
	}
	if event.Type != "permission_escalation" || event.SessionID != "session-1" || event.ToolCallID != "call-1" {
		t.Fatalf("unexpected escalation event: %#v", event)
	}
	if event.ToolTitle != "Run build" || string(event.ToolInput) != string(rawInput) {
		t.Fatalf("unexpected escalation payload: %#v", event)
	}
}

func TestEnsureReadyRecreatesSessionAfterWorkingDirChange(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransport(clientRead, clientWrite)
	serverTransport := NewTransport(serverRead, serverWrite)

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, "/new", nil, nil)
	client.transport = clientTransport
	client.running = true
	client.sessionID = "session-old"
	client.sessionCWD = "/old"

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			req, err := serverTransport.ReadMessage()
			if err != nil {
				return
			}
			switch req.Method {
			case "session/close":
				_ = serverTransport.WriteResponse(req.ID, CloseSessionResponse{})
			case "session/new":
				_ = serverTransport.WriteResponse(req.ID, NewSessionResponse{SessionID: "session-new"})
				return
			}
		}
	}()

	go func() {
		for {
			_, resp, err := clientTransport.ReadAnyMessage()
			if err != nil {
				return
			}
			if resp != nil {
				clientTransport.DeliverResponse(resp)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.EnsureReady(ctx); err != nil {
		t.Fatalf("EnsureReady error: %v", err)
	}
	<-done

	if client.sessionID != "session-new" {
		t.Fatalf("expected recreated session id, got %q", client.sessionID)
	}
	if client.sessionCWD != "/new" {
		t.Fatalf("expected recreated session cwd /new, got %q", client.sessionCWD)
	}
}

func TestNewSessionSendsEmptyMCPServersArray(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransport(clientRead, clientWrite)
	serverTransport := NewTransport(serverRead, serverWrite)

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}, "/workspace", nil, nil)
	client.transport = clientTransport
	client.running = true

	reqCh := make(chan JSONRPCRequest, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := serverTransport.ReadMessage()
		if err != nil {
			t.Errorf("server read message: %v", err)
			return
		}
		reqCh <- *req
		_ = serverTransport.WriteResponse(req.ID, NewSessionResponse{SessionID: "session-1"})
	}()

	go func() {
		for {
			_, resp, err := clientTransport.ReadAnyMessage()
			if err != nil {
				return
			}
			if resp != nil {
				clientTransport.DeliverResponse(resp)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.NewSession(ctx, "/workspace"); err != nil {
		t.Fatalf("NewSession error: %v", err)
	}
	<-done

	req := <-reqCh
	if req.Method != "session/new" {
		t.Fatalf("expected session/new request, got %q", req.Method)
	}
	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["cwd"] != "/workspace" {
		t.Fatalf("expected cwd /workspace, got %#v", params["cwd"])
	}
	rawServers, ok := params["mcpServers"]
	if !ok {
		t.Fatal("expected mcpServers field to be present")
	}
	servers, ok := rawServers.([]any)
	if !ok {
		t.Fatalf("expected mcpServers array, got %#v", rawServers)
	}
	if len(servers) != 0 {
		t.Fatalf("expected empty mcpServers array, got %#v", servers)
	}
}
