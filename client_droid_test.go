package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestEnsureReadyDoesNotCreateFactorySessionAutomatically(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransportWithProtocol(clientRead, clientWrite, WireProtocolFactoryJSONRPC)
	serverTransport := NewTransportWithProtocol(serverRead, serverWrite, WireProtocolFactoryJSONRPC)
	defer clientRead.Close()
	defer clientWrite.Close()
	defer serverRead.Close()
	defer serverWrite.Close()

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "droid", WireProtocol: WireProtocolFactoryJSONRPC}}, "/workspace", nil, nil)
	client.transport = clientTransport
	client.running = true

	reqCh := make(chan []byte, 1)
	go func() {
		raw, err := serverTransport.ReadRaw()
		if err == nil {
			reqCh <- raw
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.EnsureReady(ctx); err != nil {
		t.Fatalf("EnsureReady error: %v", err)
	}

	select {
	case raw := <-reqCh:
		t.Fatalf("expected no factory session bootstrap during EnsureReady, got %s", string(raw))
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNewSessionUsesFactoryInitializeSession(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransportWithProtocol(clientRead, clientWrite, WireProtocolFactoryJSONRPC)
	serverTransport := NewTransportWithProtocol(serverRead, serverWrite, WireProtocolFactoryJSONRPC)
	defer clientRead.Close()
	defer clientWrite.Close()
	defer serverRead.Close()
	defer serverWrite.Close()

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "droid", WireProtocol: WireProtocolFactoryJSONRPC}}, "/workspace", nil, nil)
	client.transport = clientTransport
	client.running = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := serverTransport.ReadRaw()
		if err != nil {
			t.Errorf("ReadRaw error: %v", err)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
			return
		}
		if req["method"] != droidMethodInitializeSession {
			t.Errorf("expected %s, got %#v", droidMethodInitializeSession, req["method"])
		}
		if req["type"] != "request" {
			t.Errorf("expected factory request envelope, got %#v", req["type"])
		}
		if req["factoryApiVersion"] != factoryAPIVersion {
			t.Errorf("expected factoryApiVersion %q, got %#v", factoryAPIVersion, req["factoryApiVersion"])
		}
		_ = serverTransport.WriteResponse(req["id"], FactoryInitializeSessionResponse{SessionID: "session-droid"})
	}()

	go func() {
		for {
			req, resp, err := clientTransport.ReadAnyMessage()
			if err != nil {
				return
			}
			if resp != nil {
				clientTransport.DeliverResponse(resp)
				continue
			}
			if req != nil {
				client.handleAgentRequest(context.Background(), req)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.NewSession(ctx, "/workspace"); err != nil {
		t.Fatalf("NewSession error: %v", err)
	}
	<-done

	if client.CurrentSessionID() != "session-droid" {
		t.Fatalf("expected droid session id, got %q", client.CurrentSessionID())
	}
}

func TestPromptFactorySessionCompletesOnIdle(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransportWithProtocol(clientRead, clientWrite, WireProtocolFactoryJSONRPC)
	serverTransport := NewTransportWithProtocol(serverRead, serverWrite, WireProtocolFactoryJSONRPC)
	defer clientRead.Close()
	defer clientWrite.Close()
	defer serverRead.Close()
	defer serverWrite.Close()

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "droid", WireProtocol: WireProtocolFactoryJSONRPC}}, "/workspace", nil, nil)
	client.transport = clientTransport
	client.running = true
	client.sessionID = "session-droid"
	client.sessionCWD = "/workspace"

	go func() {
		for {
			req, resp, err := clientTransport.ReadAnyMessage()
			if err != nil {
				return
			}
			if resp != nil {
				clientTransport.DeliverResponse(resp)
				continue
			}
			if req != nil {
				client.handleAgentRequest(context.Background(), req)
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := serverTransport.ReadRaw()
		if err != nil {
			t.Errorf("ReadRaw error: %v", err)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
			return
		}
		if req["method"] != droidMethodAddUserMessage {
			t.Errorf("expected %s, got %#v", droidMethodAddUserMessage, req["method"])
		}
		_ = serverTransport.WriteResponse(req["id"], map[string]any{})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{Type: droidNotificationWorkingStateChanged, NewState: "streaming_assistant_message"},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{
				Type: droidNotificationToolCall,
				ToolUse: &FactoryToolUseBlock{
					ID:    "tool-1",
					Name:  "bash",
					Input: json.RawMessage(`{"command":"pwd"}`),
				},
			},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{
				Type:      droidNotificationToolResult,
				ToolUseID: "tool-1",
				Content:   json.RawMessage(`"ok"`),
			},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{Type: droidNotificationAssistantTextDelta, TextDelta: "DROID_OK"},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{Type: droidNotificationWorkingStateChanged, NewState: droidWorkingStateIdle},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Prompt(ctx, "hello")
	if err != nil {
		t.Fatalf("Prompt error: %v", err)
	}
	<-done

	if result.Text != "DROID_OK" {
		t.Fatalf("expected streamed droid text, got %q", result.Text)
	}
	if result.StopReason != StopReasonEndTurn {
		t.Fatalf("expected stop reason %q, got %q", StopReasonEndTurn, result.StopReason)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %#v", result.ToolCalls)
	}
	if result.ToolCalls[0].Status != string(ToolCallStatusCompleted) {
		t.Fatalf("expected completed tool call, got %#v", result.ToolCalls[0])
	}
}

func TestPromptFactorySessionFallsBackToSystemErrorMessage(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransportWithProtocol(clientRead, clientWrite, WireProtocolFactoryJSONRPC)
	serverTransport := NewTransportWithProtocol(serverRead, serverWrite, WireProtocolFactoryJSONRPC)
	defer clientRead.Close()
	defer clientWrite.Close()
	defer serverRead.Close()
	defer serverWrite.Close()

	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "droid", WireProtocol: WireProtocolFactoryJSONRPC}}, "/workspace", nil, nil)
	client.transport = clientTransport
	client.running = true
	client.sessionID = "session-droid"
	client.sessionCWD = "/workspace"

	go func() {
		for {
			req, resp, err := clientTransport.ReadAnyMessage()
			if err != nil {
				return
			}
			if resp != nil {
				clientTransport.DeliverResponse(resp)
				continue
			}
			if req != nil {
				client.handleAgentRequest(context.Background(), req)
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := serverTransport.ReadRaw()
		if err != nil {
			t.Errorf("ReadRaw error: %v", err)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
			return
		}
		if req["method"] != droidMethodAddUserMessage {
			t.Errorf("expected %s, got %#v", droidMethodAddUserMessage, req["method"])
		}
		_ = serverTransport.WriteResponse(req["id"], map[string]any{})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{Type: droidNotificationWorkingStateChanged, NewState: "streaming_assistant_message"},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, map[string]any{
			"notification": map[string]any{
				"type":    droidNotificationError,
				"message": "404 status code (no body)",
			},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, map[string]any{
			"notification": map[string]any{
				"type": droidNotificationCreateMessage,
				"message": map[string]any{
					"id":         "msg-system",
					"role":       "system",
					"visibility": "user_only",
					"content": []map[string]any{
						{"type": "text", "text": "BYOK Error: 404 status code (no body)"},
					},
				},
			},
		})
		_ = serverTransport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
			Notification: FactorySessionNotificationPayload{Type: droidNotificationWorkingStateChanged, NewState: droidWorkingStateIdle},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Prompt(ctx, "hello")
	if err != nil {
		t.Fatalf("Prompt error: %v", err)
	}
	<-done

	if !strings.Contains(result.Text, "BYOK Error: 404 status code (no body)") {
		t.Fatalf("expected fallback droid error text, got %q", result.Text)
	}
	if result.StopReason != StopReasonEndTurn {
		t.Fatalf("expected stop reason %q, got %q", StopReasonEndTurn, result.StopReason)
	}
}

func TestFactoryPermissionBridgesThroughApprovalFlow(t *testing.T) {
	var written strings.Builder
	client := NewClient(DiscoveredAgent{Def: AgentDef{Name: "droid", WireProtocol: WireProtocolFactoryJSONRPC}}, t.TempDir(), nil, nil)
	client.transport = NewTransportWithProtocol(strings.NewReader(""), &written, WireProtocolFactoryJSONRPC)
	client.SetApprovalHandler(func(ctx context.Context, toolName string, input string) Decision {
		if toolName != "delegate_droid_execute" {
			t.Fatalf("unexpected toolName: %q", toolName)
		}
		if input != `{"command":"pwd"}` {
			t.Fatalf("unexpected approval input: %q", input)
		}
		return Allow
	})

	req := &JSONRPCRequest{
		ID:     "req-1",
		Method: droidMethodRequestPermission,
		Params: json.RawMessage(`{"toolUses":[{"toolUse":{"id":"tool-1","name":"bash","input":{"command":"pwd"}},"confirmationType":"exec"}],"options":[{"label":"Proceed once","value":"proceed_once"},{"label":"Cancel","value":"cancel"}]}`),
	}
	client.handleFactoryPermission(context.Background(), req)

	var resp map[string]any
	if err := json.Unmarshal([]byte(written.String()), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %#v", resp["result"])
	}
	if result["selectedOption"] != droidPermissionProceedOnce {
		t.Fatalf("expected selectedOption %q, got %#v", droidPermissionProceedOnce, result["selectedOption"])
	}
}
