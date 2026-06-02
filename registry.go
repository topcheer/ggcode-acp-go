package acp

import (
	"errors"
	"sort"
)

// AgentRegistry resolves logical agent names to launch definitions.
type AgentRegistry interface {
	List() []string
	Resolve(name string) (AgentDef, error)
	ResolveLaunchTarget(name string) (DiscoveredAgent, error)
	Discover() []DiscoveredAgent
}

// StaticAgentRegistry exposes the built-in registry with optional per-name overrides.
type StaticAgentRegistry struct {
	overrides map[string]AgentDef
}

func NewStaticAgentRegistry(overrides map[string]AgentDef) *StaticAgentRegistry {
	cloned := make(map[string]AgentDef, len(overrides))
	for key, value := range overrides {
		cloned[NormalizeAgentName(key)] = value
	}
	return &StaticAgentRegistry{overrides: cloned}
}

func (r *StaticAgentRegistry) List() []string {
	registry := MergeAgentRegistry(r.overrides)
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *StaticAgentRegistry) Resolve(name string) (AgentDef, error) {
	def, ok := ResolveAgentDefinition(name, r.overrides)
	if !ok {
		return AgentDef{}, errors.New("unknown agent: " + name)
	}
	return def, nil
}

func (r *StaticAgentRegistry) ResolveLaunchTarget(name string) (DiscoveredAgent, error) {
	return FindLaunchTarget(name, r.overrides)
}

func (r *StaticAgentRegistry) Discover() []DiscoveredAgent {
	registry := MergeAgentRegistry(r.overrides)
	defs := make([]AgentDef, 0, len(registry))
	for _, def := range registry {
		defs = append(defs, def)
	}
	return DiscoverWithDefs(defs)
}
