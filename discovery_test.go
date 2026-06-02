package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestDiscoverEmpty(t *testing.T) {
	// With an empty PATH, nothing should be found
	t.Setenv("PATH", t.TempDir())
	agents := Discover()
	if len(agents) != 0 {
		t.Errorf("expected 0 agents with empty PATH, got %d", len(agents))
	}
}

func TestDiscoverKnownAgents(t *testing.T) {
	// Create a temp dir with fake agent binaries
	dir := t.TempDir()

	// Create fake "copilot" binary
	for _, name := range []string{"copilot", "droid", "opencode"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Add an unrelated binary
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Set PATH to our temp dir
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir)
	defer t.Setenv("PATH", origPath)

	// Clear exec.LookPath cache by using the environment directly
	agents := Discover()

	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Def.Name
	}
	sort.Strings(names)

	expected := []string{"copilot", "droid", "opencode"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d agents, got %d: %v", len(expected), len(names), names)
	}
	for i, name := range expected {
		if names[i] != name {
			t.Errorf("expected agent[%d] = %q, got %q", i, name, names[i])
		}
	}

	// Verify paths are correct
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if filepath.Dir(a.Path) != resolvedDir {
			t.Errorf("agent %q path = %q, expected in %q", a.Def.Name, a.Path, resolvedDir)
		}
	}
}

func TestDiscoverPartial(t *testing.T) {
	// Only create "droid"
	dir := t.TempDir()
	path := filepath.Join(dir, "droid")
	if err := os.WriteFile(path, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir)
	defer t.Setenv("PATH", origPath)

	agents := Discover()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Def.Name != "droid" {
		t.Errorf("expected agent name = %q, got %q", "droid", agents[0].Def.Name)
	}
}

func TestFindBinaryNotFound(t *testing.T) {
	_, err := findBinary([]string{"nonexistent_binary_xyz_12345"})
	if err == nil {
		t.Error("expected error for nonexistent binary")
	}
	if !os.IsNotExist(err) && err != exec.ErrNotFound {
		// exec.LookPath returns exec.ErrNotFound on some platforms
		t.Logf("got error: %v (acceptable)", err)
	}
}

func TestFindBinaryRejectsWorkspaceLocalPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	name := "copilot-local-test"
	path := filepath.Join(wd, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	t.Setenv("PATH", wd)
	if _, err := findBinary([]string{name}); err == nil {
		t.Fatal("expected workspace-local binary to be rejected")
	}
}

func TestKnownAgentsStructure(t *testing.T) {
	// Verify KnownAgents has expected entries
	names := make(map[string]bool)
	for _, def := range KnownAgents {
		if def.Name == "" {
			t.Error("AgentDef has empty Name")
		}
		if def.Title == "" {
			t.Errorf("AgentDef %q has empty Title", def.Name)
		}
		if def.Command == "" {
			t.Errorf("AgentDef %q has no Command", def.Name)
		}
		if len(def.CheckBinaries) == 0 {
			t.Errorf("AgentDef %q has no CheckBinaries", def.Name)
		}
		if names[def.Name] {
			t.Errorf("duplicate agent name: %q", def.Name)
		}
		names[def.Name] = true
	}

	expected := []string{"claude", "codex", "copilot", "cursor", "droid", "fast-agent", "gemini", "ggcode", "kimi", "kilocode", "kiro", "opencode", "pi", "qoder", "qwen", "trae"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected agent %q in KnownAgents", name)
		}
	}
}

func TestKiloCodeLaunchUsesAcpCapablePackage(t *testing.T) {
	for _, def := range KnownAgents {
		if def.Name != "kilocode" {
			continue
		}
		if len(def.Args) < 2 || def.Args[1] != "@kilocode/cli@rc" {
			t.Fatalf("expected kilocode package exec to pin @kilocode/cli@rc, got %v", def.Args)
		}
		return
	}
	t.Fatal("kilocode agent definition not found")
}

func TestCursorLaunchPrefersOfficialAgentBinary(t *testing.T) {
	for _, def := range KnownAgents {
		if def.Name != "cursor" {
			continue
		}
		if def.Command != "agent" {
			t.Fatalf("expected cursor command to prefer agent, got %q", def.Command)
		}
		if len(def.CheckBinaries) < 2 || def.CheckBinaries[0] != "agent" || def.CheckBinaries[1] != "cursor-agent" {
			t.Fatalf("expected cursor binary search order [agent cursor-agent], got %v", def.CheckBinaries)
		}
		return
	}
	t.Fatal("cursor agent definition not found")
}

func TestKiroLaunchPrefersOfficialCLIName(t *testing.T) {
	for _, def := range KnownAgents {
		if def.Name != "kiro" {
			continue
		}
		if def.Command != "kiro-cli" {
			t.Fatalf("expected kiro command to prefer kiro-cli, got %q", def.Command)
		}
		if len(def.CheckBinaries) < 2 || def.CheckBinaries[0] != "kiro-cli" || def.CheckBinaries[1] != "kiro-cli-chat" {
			t.Fatalf("expected kiro binary search order [kiro-cli kiro-cli-chat], got %v", def.CheckBinaries)
		}
		return
	}
	t.Fatal("kiro agent definition not found")
}

func TestDroidLaunchUsesFactoryStreamJSONRPC(t *testing.T) {
	for _, def := range KnownAgents {
		if def.Name != "droid" {
			continue
		}
		if def.WireProtocol != WireProtocolFactoryJSONRPC {
			t.Fatalf("expected droid wire protocol %q, got %q", WireProtocolFactoryJSONRPC, def.WireProtocol)
		}
		expected := []string{"exec", "--input-format", "stream-jsonrpc", "--output-format", "stream-jsonrpc"}
		if strings.Join(def.Args, " ") != strings.Join(expected, " ") {
			t.Fatalf("expected droid args %v, got %v", expected, def.Args)
		}
		return
	}
	t.Fatal("droid agent definition not found")
}
