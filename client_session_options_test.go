package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestResolvedAgentArgsAddsQoderSessionOptions(t *testing.T) {
	agent := DiscoveredAgent{
		Def:  AgentDef{Name: "qoder"},
		Args: []string{"--acp"},
	}
	args := resolvedAgentArgs(agent, &SessionOptions{
		AllowedTools: []string{"bash", "read", "custom"},
		MaxTurns:     7,
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--max-turns=7") {
		t.Fatalf("expected qoder max-turns injection, got %v", args)
	}
	if !strings.Contains(joined, "--allowed-tools=BASH,READ,custom") {
		t.Fatalf("expected qoder allowed-tools injection, got %v", args)
	}
}

func TestBuildSessionOptionsMetaAddsClaudeOptions(t *testing.T) {
	meta := buildSessionOptionsMeta(DiscoveredAgent{Def: AgentDef{Name: "claude"}}, &SessionOptions{
		Model:        "sonnet",
		AllowedTools: []string{"read_file"},
		MaxTurns:     3,
		SystemPrompt: SystemPromptOption{Append: "extra"},
	})
	if len(meta) == 0 {
		t.Fatalf("expected meta payload")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(meta, &payload); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	claudeCode, ok := payload["claudeCode"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected claudeCode payload, got %#v", payload["claudeCode"])
	}
	options, ok := claudeCode["options"].(map[string]interface{})
	if !ok || options["model"] != "sonnet" {
		t.Fatalf("expected claudeCode.options.model, got %#v", claudeCode["options"])
	}
	systemPrompt, ok := payload["systemPrompt"].(map[string]interface{})
	if !ok || systemPrompt["append"] != "extra" {
		t.Fatalf("expected appended system prompt, got %#v", payload["systemPrompt"])
	}
}

func TestNewSessionIncludesClaudeSessionOptionsMeta(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	clientTransport := NewTransport(clientRead, clientWrite)
	serverTransport := NewTransport(serverRead, serverWrite)

	received := make(chan NewSessionRequest, 1)
	go func() {
		req, err := serverTransport.ReadMessage()
		if err != nil {
			t.Errorf("read session/new request: %v", err)
			return
		}
		var params NewSessionRequest
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("unmarshal session/new: %v", err)
			return
		}
		received <- params
		if err := serverTransport.WriteResponse(req.ID, NewSessionResponse{SessionID: "session-1"}); err != nil {
			t.Errorf("write session/new response: %v", err)
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

	acpClient := NewClientWithOptions(
		DiscoveredAgent{Def: AgentDef{Name: "claude"}},
		t.TempDir(),
		nil,
		nil,
		ClientOptions{SessionOptions: &SessionOptions{SystemPrompt: "system", Model: "sonnet"}},
	)
	acpClient.transport = clientTransport
	acpClient.running = true
	acpClient.initialized = true
	acpClient.done = make(chan struct{})

	if err := acpClient.NewSession(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("NewSession error: %v", err)
	}

	select {
	case params := <-received:
		if len(params.Meta) == 0 {
			t.Fatalf("expected session/new meta")
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(params.Meta, &payload); err != nil {
			t.Fatalf("unmarshal meta: %v", err)
		}
		if payload["systemPrompt"] != "system" {
			t.Fatalf("expected system prompt in meta, got %#v", payload["systemPrompt"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for session/new")
	}
}
