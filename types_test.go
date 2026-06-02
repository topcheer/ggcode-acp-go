package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewSessionRequestMarshalIncludesEmptyMCPServers(t *testing.T) {
	payload, err := json.Marshal(NewSessionRequest{
		CWD: "/tmp/project",
	})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if !strings.Contains(string(payload), `"mcpServers":[]`) {
		t.Fatalf("expected mcpServers empty array, got %s", payload)
	}
}

func TestResumeSessionRequestMarshalIncludesEmptyMCPServers(t *testing.T) {
	payload, err := json.Marshal(ResumeSessionRequest{
		SessionID: "session-1",
		CWD:       "/tmp/project",
	})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if !strings.Contains(string(payload), `"mcpServers":[]`) {
		t.Fatalf("expected mcpServers empty array, got %s", payload)
	}
}

func TestAuthMethodUnmarshalAcceptsEmptyObjectEnv(t *testing.T) {
	var method AuthMethod
	if err := json.Unmarshal([]byte(`{
		"id":"pi_terminal_login",
		"name":"Launch pi in the terminal",
		"type":"terminal",
		"args":["--terminal-login"],
		"env":{}
	}`), &method); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(method.Env) != 0 {
		t.Fatalf("expected no env vars, got %+v", method.Env)
	}
}

func TestAuthMethodUnmarshalAcceptsMappedEnv(t *testing.T) {
	var method AuthMethod
	if err := json.Unmarshal([]byte(`{
		"id":"example",
		"name":"Example",
		"env":{
			"FOO":"bar",
			"BAZ":{"value":"qux"}
		}
	}`), &method); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(method.Env) != 2 {
		t.Fatalf("expected 2 env vars, got %+v", method.Env)
	}
	if method.Env[0].Name != "BAZ" || method.Env[0].Value != "qux" {
		t.Fatalf("expected BAZ=qux first, got %+v", method.Env[0])
	}
	if method.Env[1].Name != "FOO" || method.Env[1].Value != "bar" {
		t.Fatalf("expected FOO=bar second, got %+v", method.Env[1])
	}
}
