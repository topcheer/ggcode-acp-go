package acp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
)

type Decision int

const (
	Allow Decision = iota
	Deny
	Ask
)

type PermissionMode string

const (
	AutoMode       PermissionMode = "auto"
	SupervisedMode PermissionMode = "supervised"
	BypassMode     PermissionMode = "bypass"
)

// PermissionPolicy determines whether a tool call needs approval.
// ggcode's richer permission policy satisfies this interface directly.
type PermissionPolicy interface {
	Check(toolName string, input json.RawMessage) (Decision, error)
	AllowedPathForTool(toolName, path string) bool
}

// ConfigPolicy is a minimal standalone permission policy implementation for
// standalone consumers and package tests.
type ConfigPolicy struct {
	mu          sync.RWMutex
	mode        PermissionMode
	overrides   map[string]Decision
	allowedDirs []string
}

func NewConfigPolicyWithMode(overrides map[string]Decision, allowedDirs []string, mode PermissionMode) *ConfigPolicy {
	cloned := make(map[string]Decision, len(overrides))
	for key, value := range overrides {
		cloned[key] = value
	}
	return &ConfigPolicy{
		mode:        mode,
		overrides:   cloned,
		allowedDirs: append([]string(nil), allowedDirs...),
	}
}

func (p *ConfigPolicy) Check(toolName string, _ json.RawMessage) (Decision, error) {
	if p == nil {
		return Ask, nil
	}
	p.mu.RLock()
	override, ok := p.overrides[toolName]
	mode := p.mode
	p.mu.RUnlock()
	if ok {
		return override, nil
	}
	if mode == BypassMode {
		return Allow, nil
	}
	return Ask, nil
}

func (p *ConfigPolicy) AllowedPathForTool(_ string, path string) bool {
	if p == nil {
		return true
	}
	p.mu.RLock()
	allowedDirs := append([]string(nil), p.allowedDirs...)
	p.mu.RUnlock()
	if len(allowedDirs) == 0 {
		return true
	}
	target := filepath.Clean(path)
	for _, dir := range allowedDirs {
		base := filepath.Clean(dir)
		if base == "." || base == "" {
			continue
		}
		if target == base || strings.HasPrefix(target, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (p *ConfigPolicy) SetOverride(toolName string, decision Decision) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.overrides == nil {
		p.overrides = make(map[string]Decision)
	}
	p.overrides[toolName] = decision
}
