package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// AgentDef describes a known ACP-capable CLI launch target.
type AgentDef struct {
	Name           string
	Title          string
	Description    string
	Command        string
	Args           []string
	CheckBinaries  []string
	Aliases        []string
	SessionSupport string
	WireProtocol   WireProtocol
}

// DiscoveredAgent represents an available launch target on the local system.
type DiscoveredAgent struct {
	Def     AgentDef
	Path    string
	Command string
	Args    []string
	Source  string
}

var agentAliases = map[string]string{
	"factory-droid": "droid",
	"factorydroid":  "droid",
}

// KnownAgents mirrors the built-in CLI registry used by the ACP headless runtime.
var KnownAgents = []AgentDef{
	{
		Name:          "pi",
		Title:         "Pi",
		Description:   "Pi ACP adapter launched via npx",
		Command:       "npx",
		Args:          []string{"pi-acp@^0.0.26"},
		CheckBinaries: []string{"npx"},
	},
	{
		Name:          "codex",
		Title:         "Codex",
		Description:   "Codex ACP adapter launched via npx",
		Command:       "npx",
		Args:          []string{"-y", "@agentclientprotocol/codex-acp@^0.0.44"},
		CheckBinaries: []string{"npx"},
	},
	{
		Name:          "claude",
		Title:         "Claude",
		Description:   "Claude ACP adapter launched via npx",
		Command:       "npx",
		Args:          []string{"-y", "@agentclientprotocol/claude-agent-acp@^0.37.0"},
		CheckBinaries: []string{"npx"},
	},
	{
		Name:          "gemini",
		Title:         "Gemini",
		Description:   "Gemini CLI in ACP mode",
		Command:       "gemini",
		Args:          []string{"--acp"},
		CheckBinaries: []string{"gemini"},
	},
	{
		Name:          "cursor",
		Title:         "Cursor",
		Description:   "Cursor agent ACP entrypoint",
		Command:       "agent",
		Args:          []string{"acp"},
		CheckBinaries: []string{"agent", "cursor-agent"},
	},
	{
		Name:          "copilot",
		Title:         "GitHub Copilot",
		Description:   "GitHub Copilot CLI agent mode",
		Command:       "copilot",
		Args:          []string{"--acp", "--stdio"},
		CheckBinaries: []string{"copilot"},
	},
	{
		Name:           "droid",
		Title:          "Droid",
		Description:    "Droid Factory stream-jsonrpc execution mode",
		Command:        "droid",
		Args:           []string{"exec", "--input-format", "stream-jsonrpc", "--output-format", "stream-jsonrpc"},
		CheckBinaries:  []string{"droid"},
		Aliases:        []string{"factory-droid", "factorydroid"},
		SessionSupport: "persistent",
		WireProtocol:   WireProtocolFactoryJSONRPC,
	},
	{
		Name:          "fast-agent",
		Title:         "Fast Agent",
		Description:   "Fast Agent MCP ACP bridge",
		Command:       "uvx",
		Args:          []string{"fast-agent-mcp", "acp"},
		CheckBinaries: []string{"uvx"},
	},
	{
		Name:          "kilocode",
		Title:         "KiloCode",
		Description:   "KiloCode ACP entrypoint launched via npx",
		Command:       "npx",
		Args:          []string{"-y", "@kilocode/cli@rc", "acp"},
		CheckBinaries: []string{"npx"},
	},
	{
		Name:          "kimi",
		Title:         "Kimi",
		Description:   "Kimi ACP entrypoint",
		Command:       "kimi",
		Args:          []string{"acp"},
		CheckBinaries: []string{"kimi"},
	},
	{
		Name:          "kiro",
		Title:         "Kiro",
		Description:   "Kiro CLI ACP entrypoint",
		Command:       "kiro-cli",
		Args:          []string{"acp"},
		CheckBinaries: []string{"kiro-cli", "kiro-cli-chat"},
	},
	{
		Name:          "opencode",
		Title:         "OpenCode",
		Description:   "OpenCode ACP entrypoint launched via npx",
		Command:       "npx",
		Args:          []string{"-y", "opencode-ai", "acp"},
		CheckBinaries: []string{"npx", "opencode"},
	},
	{
		Name:          "qoder",
		Title:         "Qoder",
		Description:   "Qoder CLI ACP mode",
		Command:       "qodercli",
		Args:          []string{"--acp"},
		CheckBinaries: []string{"qodercli"},
	},
	{
		Name:          "qwen",
		Title:         "Qwen",
		Description:   "Qwen CLI ACP mode",
		Command:       "qwen",
		Args:          []string{"--acp"},
		CheckBinaries: []string{"qwen"},
	},
	{
		Name:          "trae",
		Title:         "Trae",
		Description:   "Trae CLI ACP server mode",
		Command:       "traecli",
		Args:          []string{"acp", "serve"},
		CheckBinaries: []string{"traecli"},
	},
	{
		Name:          "ggcode",
		Title:         "ggcode",
		Description:   "ggcode running in ACP server mode",
		Command:       "ggcode",
		Args:          []string{"acp"},
		CheckBinaries: []string{"ggcode"},
	},
}

func NormalizeAgentName(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func MergeAgentRegistry(overrides map[string]AgentDef) map[string]AgentDef {
	merged := make(map[string]AgentDef, len(KnownAgents)+len(overrides))
	for _, def := range KnownAgents {
		merged[def.Name] = def
		for _, alias := range def.Aliases {
			agentAliases[NormalizeAgentName(alias)] = def.Name
		}
	}
	for key, value := range overrides {
		normalized := NormalizeAgentName(key)
		if normalized == "" || value.Command == "" {
			continue
		}
		value.Name = normalized
		merged[normalized] = value
	}
	return merged
}

func ResolveAgentDefinition(agentName string, overrides map[string]AgentDef) (AgentDef, bool) {
	registry := MergeAgentRegistry(overrides)
	normalized := NormalizeAgentName(agentName)
	if normalized == "" {
		return AgentDef{}, false
	}
	if def, ok := registry[normalized]; ok {
		return def, true
	}
	if alias, ok := agentAliases[normalized]; ok {
		def, ok := registry[alias]
		return def, ok
	}
	return AgentDef{}, false
}

func ListBuiltInAgents() []string {
	names := make([]string, 0, len(KnownAgents))
	for _, def := range KnownAgents {
		names = append(names, def.Name)
	}
	sort.Strings(names)
	return names
}

// Discover scans the built-in registry and returns locally usable launch targets.
func Discover() []DiscoveredAgent {
	return DiscoverWithDefs(KnownAgents)
}

// DiscoverWithDefs scans the local system for the provided launch definitions.
func DiscoverWithDefs(defs []AgentDef) []DiscoveredAgent {
	var found []DiscoveredAgent
	for _, def := range defs {
		agent, err := discoverAgent(def)
		if err != nil {
			continue
		}
		found = append(found, agent)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Def.Name < found[j].Def.Name })
	return found
}

func discoverAgent(def AgentDef) (DiscoveredAgent, error) {
	candidates := append([]string(nil), def.CheckBinaries...)
	if len(candidates) == 0 && def.Command != "" {
		candidates = []string{def.Command}
	}
	path, err := findBinary(candidates)
	if err != nil {
		return DiscoveredAgent{}, err
	}
	agent := DiscoveredAgent{
		Def:     def,
		Path:    path,
		Command: path,
		Args:    append([]string(nil), def.Args...),
		Source:  "installed",
	}
	debugLogf("discovered agent %q via %s", def.Name, path)
	return agent, nil
}

// FindLaunchTarget resolves a specific agent name to a launchable local target.
func FindLaunchTarget(agentName string, overrides map[string]AgentDef) (DiscoveredAgent, error) {
	def, ok := ResolveAgentDefinition(agentName, overrides)
	if !ok {
		return DiscoveredAgent{}, exec.ErrNotFound
	}
	return discoverAgent(def)
}

// findBinary searches for the first executable candidate in $PATH.
func findBinary(names []string) (string, error) {
	cwd, _ := os.Getwd()
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(path) {
			debugLogf("skipping %q at non-absolute path %q", name, path)
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != "" {
			path = resolved
		}
		if cwd != "" {
			if rel, err := filepath.Rel(cwd, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				debugLogf("skipping %q at workspace-local path %q", name, path)
				continue
			}
		}
		return path, nil
	}
	return "", exec.ErrNotFound
}
