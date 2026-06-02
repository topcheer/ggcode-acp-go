package acp

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestResolveGeminiCommandArgsUsesExperimentalFlagForLegacyVersion(t *testing.T) {
	got := resolveGeminiCommandArgsWithReader("gemini", []string{"--acp"}, func(string, []string, time.Duration) (string, error) {
		return "gemini-cli 0.32.1", nil
	})
	if !reflect.DeepEqual(got, []string{"--experimental-acp"}) {
		t.Fatalf("expected legacy gemini args rewrite, got %v", got)
	}
}

func TestResolveGeminiCommandArgsKeepsModernFlag(t *testing.T) {
	want := []string{"--acp"}
	got := resolveGeminiCommandArgsWithReader("gemini", append([]string(nil), want...), func(string, []string, time.Duration) (string, error) {
		return "gemini-cli 0.33.0", nil
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected modern gemini args unchanged, got %v", got)
	}
}

func TestEnsureCopilotAcpSupportRejectsOldHelpOutput(t *testing.T) {
	err := ensureCopilotAcpSupportWithReader("copilot", []string{"--acp", "--stdio"}, func(string, []string, time.Duration) (string, error) {
		return "Usage: copilot [command]\n  no acp here", nil
	})
	if err == nil {
		t.Fatal("expected unsupported copilot error")
	}
}

func TestEnsureCopilotAcpSupportIgnoresProbeErrors(t *testing.T) {
	if err := ensureCopilotAcpSupportWithReader("copilot", []string{"--acp", "--stdio"}, func(string, []string, time.Duration) (string, error) {
		return "", errors.New("boom")
	}); err != nil {
		t.Fatalf("expected probe errors to be ignored, got %v", err)
	}
}

func TestClaudeSessionCreateTimeoutHonorsEnv(t *testing.T) {
	t.Setenv("GGCODE_ACP_CLAUDE_SESSION_CREATE_TIMEOUT_MS", "12345")
	if got := claudeSessionCreateTimeout(DiscoveredAgent{Def: AgentDef{Name: "claude"}}); got != 12345*time.Millisecond {
		t.Fatalf("unexpected timeout: %v", got)
	}
}

func TestClaudeSessionCreateTimeoutDefaultsForNonClaude(t *testing.T) {
	os.Unsetenv("GGCODE_ACP_CLAUDE_SESSION_CREATE_TIMEOUT_MS")
	if got := claudeSessionCreateTimeout(DiscoveredAgent{Def: AgentDef{Name: "copilot"}}); got != 30*time.Second {
		t.Fatalf("unexpected non-claude timeout: %v", got)
	}
}
