package acp

import (
	"context"
	"sync"
)

// ClientManager manages the lifecycle of all ACP agent clients.
type ClientManager struct {
	clients      map[string]*Client // keyed by agent name
	discoveries  map[string]DiscoveredAgent
	mu           sync.RWMutex
	workingDir   string
	policy       PermissionPolicy
	mcpServers   []MCPServer
	onPermission PermissionHandler
	onApproval   ApprovalHandler
}

type ClientManagerOptions struct {
	WorkingDir string
	Policy     PermissionPolicy
	MCPServers []MCPServer
	Agents     []DiscoveredAgent
}

// NewClientManager discovers ACP agents and stores their shared startup config.
// ACP delegates intentionally send an empty mcpServers array because MCP
// passthrough is disabled for stability.
func NewClientManager(workingDir string, policy PermissionPolicy) *ClientManager {
	return NewClientManagerWithOptions(ClientManagerOptions{
		WorkingDir: workingDir,
		Policy:     policy,
		MCPServers: []MCPServer{},
	})
}

func NewClientManagerWithOptions(opts ClientManagerOptions) *ClientManager {
	mgr := &ClientManager{
		clients:     make(map[string]*Client),
		discoveries: make(map[string]DiscoveredAgent),
		workingDir:  opts.WorkingDir,
		policy:      opts.Policy,
		mcpServers:  cloneMCPServers(opts.MCPServers),
	}

	agents := opts.Agents
	if len(agents) == 0 {
		agents = Discover()
	}
	for _, agent := range agents {
		mgr.registerLocked(agent)
	}

	return mgr
}

func (m *ClientManager) registerLocked(agent DiscoveredAgent) {
	m.discoveries[agent.Def.Name] = agent
	m.clients[agent.Def.Name] = NewClient(agent, m.workingDir, m.policy, m.mcpServers)
	debugLogf("registered agent %q (%s)", agent.Def.Name, agent.Path)
}

func (m *ClientManager) Register(agent DiscoveredAgent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerLocked(agent)
}

func (m *ClientManager) SetWorkingDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workingDir = dir
	for _, client := range m.clients {
		client.SetWorkingDir(dir)
	}
}

func (m *ClientManager) SetPermissionHandler(h PermissionHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPermission = h
	for _, client := range m.clients {
		client.SetPermissionHandler(h)
	}
}

func (m *ClientManager) SetApprovalHandler(h ApprovalHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onApproval = h
	for _, client := range m.clients {
		client.SetApprovalHandler(h)
	}
}

// Available returns the list of available agent names.
func (m *ClientManager) Available() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.discoveries))
	for name := range m.discoveries {
		names = append(names, name)
	}
	return names
}

// AgentInfo returns display information for an agent.
func (m *ClientManager) AgentInfo(name string) (title, description string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.discoveries[name]
	if !ok {
		return "", "", false
	}
	return d.Def.Title, d.Def.Description, true
}

// CloseAll shuts down all running agent processes.
func (m *ClientManager) CloseAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for name, client := range m.clients {
		if err := client.Close(); err != nil {
			debugLogf("error closing agent %q: %v", name, err)
		}
	}
}

func (m *ClientManager) newClient(name string) (*Client, error) {
	m.mu.RLock()
	discovery, ok := m.discoveries[name]
	workingDir := m.workingDir
	onPermission := m.onPermission
	onApproval := m.onApproval
	policy := m.policy
	mcpServers := cloneMCPServers(m.mcpServers)
	m.mu.RUnlock()
	if !ok {
		return nil, ErrAgentNotFound{name: name}
	}
	client := NewClient(discovery, workingDir, policy, mcpServers)
	client.SetPermissionHandler(onPermission)
	client.SetApprovalHandler(onApproval)
	return client, nil
}

func (m *ClientManager) Get(ctx context.Context, name string) (*Client, error) {
	client, err := m.newClient(name)
	if err != nil {
		return nil, err
	}
	if err := client.EnsureReady(ctx); err != nil {
		return nil, err
	}
	return client, nil
}
