package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// PromptResult is the aggregated result of a prompt execution.
type PromptResult struct {
	Text       string            // agent's text response
	StopReason StopReason        // why the agent stopped
	ToolCalls  []ToolCallSummary // summary of tool calls made
}

// ToolCallSummary is a simplified view of a tool call for display.
type ToolCallSummary struct {
	Name   string
	Title  string
	Status string // "completed", "failed"
}

// PermissionHandler is called when the agent requests permission.
// Return the response to send back to the agent.
type PermissionHandler func(ctx context.Context, req RequestPermissionRequest) (RequestPermissionResponse, error)

// ApprovalHandler is the host-side interactive approval bridge used when ACP
// requests need to flow through ggcode's existing approval UX.
type ApprovalHandler func(ctx context.Context, toolName string, input string) Decision
type PermissionEscalationHandler func(PermissionEscalationEvent)

type promptToolState struct {
	toolID         string
	toolName       string
	title          string
	kind           ToolKind
	status         ToolCallStatus
	rawInput       json.RawMessage
	rawOutput      json.RawMessage
	content        interface{}
	locations      []ToolCallLocation
	startedEmitted bool
}

type ClientOptions struct {
	SessionOptions *SessionOptions
}

const (
	defaultPromptIdleTimeout    = 5 * time.Minute
	defaultPromptRequestTimeout = 30 * time.Minute
	defaultCloseSessionTimeout  = 1 * time.Second
	defaultCopilotHelpTimeout   = 2 * time.Second
	defaultClaudeSessionTimeout = 60 * time.Second
)

// Client manages a single ACP agent process.
// It handles lifecycle (start/stop), session management, and prompt execution.
type Client struct {
	def        DiscoveredAgent
	workingDir string
	policy     PermissionPolicy
	mcpServers []MCPServer
	options    ClientOptions

	// Process management
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	transport  *Transport
	cancelProc context.CancelFunc

	// State
	mu                   sync.Mutex
	setupMu              sync.Mutex
	execMu               sync.Mutex
	initialized          bool
	caps                 AgentCapabilities
	authMethods          []AuthMethod
	agentInfo            ImplementationInfo
	sessionID            string
	sessionCWD           string
	sessionTitle         string
	sessionModes         *SessionModeState
	sessionConfigOptions []SessionConfigOption
	availableCommands    []AvailableCommand
	running              bool

	// Permission handling
	onPermission PermissionHandler
	onApproval   ApprovalHandler
	onEscalation PermissionEscalationHandler

	// Read loop management
	cancelRead context.CancelFunc
	done       chan struct{}
	readErr    error
	stderrTail outputTail
	activity   activityTrail

	// Prompt execution state
	promptMu               sync.Mutex
	promptText             strings.Builder
	promptTools            []ToolCallSummary
	promptToolMeta         map[string]*promptToolState
	activePromptID         string
	promptDone             chan PromptResponse
	promptActivity         chan struct{}
	promptOnEvent          func(PromptEvent)
	rawOnMessage           func(json.RawMessage)
	promptIdleTime         time.Duration
	promptReqTime          time.Duration
	droidSeenNonIdle       bool
	droidLastAssistantText string
	droidLastFallbackText  string
}

// NewClient creates a new ACP client for the given discovered agent.
func NewClient(agent DiscoveredAgent, workingDir string, policy PermissionPolicy, mcpServers []MCPServer) *Client {
	return NewClientWithOptions(agent, workingDir, policy, mcpServers, ClientOptions{})
}

// NewClientWithOptions creates a new ACP client with optional session bootstrap settings.
func NewClientWithOptions(agent DiscoveredAgent, workingDir string, policy PermissionPolicy, mcpServers []MCPServer, options ClientOptions) *Client {
	return &Client{
		def:            agent,
		workingDir:     workingDir,
		policy:         policy,
		mcpServers:     cloneMCPServers(mcpServers),
		options:        ClientOptions{SessionOptions: cloneSessionOptions(options.SessionOptions)},
		done:           make(chan struct{}),
		promptIdleTime: defaultPromptIdleTimeout,
		promptReqTime:  defaultPromptRequestTimeout,
	}
}

func (c *Client) Prompt(ctx context.Context, prompt string) (*PromptResult, error) {
	return c.promptInternal(ctx, prompt, nil)
}

func (c *Client) PromptStream(ctx context.Context, prompt string, onEvent func(PromptEvent)) (*PromptResult, error) {
	return c.promptInternal(ctx, prompt, onEvent)
}

// CurrentSessionID returns the active ACP session id tracked by this client.
func (c *Client) CurrentSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// CurrentSessionState returns the cached session metadata learned from ACP responses and notifications.
func (c *Client) CurrentSessionState() (title string, modes *SessionModeState, configOptions []SessionConfigOption, commands []AvailableCommand) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionTitle, cloneSessionModeState(c.sessionModes), cloneSessionConfigOptions(c.sessionConfigOptions), cloneAvailableCommands(c.availableCommands)
}

// SetPermissionHandler sets the handler for agent permission requests.
func (c *Client) SetPermissionHandler(h PermissionHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onPermission = h
}

func (c *Client) SetApprovalHandler(h ApprovalHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onApproval = h
}

func (c *Client) SetPermissionEscalationHandler(h PermissionEscalationHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEscalation = h
}

func (c *Client) SetRawMessageHandler(h func(json.RawMessage)) {
	c.mu.Lock()
	c.rawOnMessage = h
	transport := c.transport
	c.mu.Unlock()
	if transport != nil {
		transport.SetRawMessageObserver(h)
	}
}

func (c *Client) SetWorkingDir(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workingDir = dir
	if c.cmd != nil {
		c.cmd.Dir = dir
	}
}

// Name returns the canonical agent name.
func (c *Client) Name() string { return c.def.Def.Name }

// Title returns the display name.
func (c *Client) Title() string { return c.def.Def.Title }

// Description returns the agent description.
func (c *Client) Description() string { return c.def.Def.Description }

// Capabilities returns the agent's declared capabilities (after initialize).
func (c *Client) Capabilities() AgentCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// Start launches the agent process and performs ACP initialize handshake.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	workingDir := c.workingDir
	c.mu.Unlock()

	args := resolvedAgentArgs(c.def, c.options.SessionOptions)
	args = resolveGeminiCommandArgs(c.def.Command, args)
	if err := ensureCopilotAcpSupport(c.def.Command, args); err != nil {
		return err
	}
	debugLogf("starting agent %q: %s %s", c.def.Def.Name, c.def.Command, strings.Join(args, " "))

	c.stderrTail.Reset()

	procCtx, cancelProc := context.WithCancel(context.Background())
	extraEnv, err := launchEnvForAgent(c.def.Def.Name)
	if err != nil {
		cancelProc()
		return err
	}
	cmd := exec.CommandContext(procCtx, c.def.Command, args...)
	cmd.Dir = workingDir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.Stderr = &c.stderrTail
	configureACPCommandProcess(cmd)

	// Wire stdin/stdout
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancelProc()
		return fmt.Errorf("creating stdin pipe for %s: %w", c.def.Def.Name, err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		cancelProc()
		return fmt.Errorf("creating stdout pipe for %s: %w", c.def.Def.Name, err)
	}

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		cancelProc()
		return fmt.Errorf("starting process %s: %w", c.def.Def.Name, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdinPipe
	c.transport = NewTransportWithProtocol(stdoutPipe, stdinPipe, c.def.Def.WireProtocol)
	c.transport.SetRawMessageObserver(c.handleRawMessage)
	c.cancelProc = cancelProc
	c.readErr = nil

	// Start read loop
	readCtx, cancelRead := context.WithCancel(context.Background())
	c.cancelRead = cancelRead
	c.done = make(chan struct{})
	c.mu.Unlock()
	go c.readLoop(readCtx)

	// Perform initialize handshake when the protocol requires it.
	if !c.usesFactoryProtocol() {
		if err := c.initialize(ctx); err != nil {
			cancelRead()
			cancelProc()
			_ = killACPProcess(cmd)
			_ = cmd.Wait()
			c.mu.Lock()
			c.cancelRead = nil
			c.cancelProc = nil
			c.cmd = nil
			c.stdin = nil
			c.transport = nil
			c.mu.Unlock()
			return fmt.Errorf("initialize handshake with %s: %w", c.def.Def.Name, err)
		}
	} else {
		c.mu.Lock()
		c.caps = AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: &SessionCapabilities{
				Close:  &SessionCloseCapabilities{},
				Resume: &SessionResumeCapabilities{},
			},
		}
		c.authMethods = nil
		c.agentInfo = ImplementationInfo{
			Name:    c.def.Def.Name,
			Title:   c.def.Def.Title,
			Version: factoryProtocolVersion,
		}
		c.initialized = true
		c.mu.Unlock()
	}

	c.mu.Lock()
	c.running = true
	c.mu.Unlock()

	debugLogf("agent %q started successfully (protocol=%d, loadSession=%v)",
		c.def.Def.Name, ProtocolVersion, c.caps.LoadSession)

	return nil
}

func (c *Client) handleRawMessage(message json.RawMessage) {
	c.mu.Lock()
	hook := c.rawOnMessage
	c.mu.Unlock()
	if hook == nil {
		return
	}
	hook(message)
}

func (c *Client) EnsureReady(ctx context.Context) error {
	c.setupMu.Lock()
	defer c.setupMu.Unlock()

	if err := c.Start(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	sessionID := c.sessionID
	sessionCWD := c.sessionCWD
	workingDir := c.workingDir
	c.mu.Unlock()
	if c.usesFactoryProtocol() {
		if sessionID != "" && sessionCWD != workingDir {
			c.closeSession(sessionID)
			c.mu.Lock()
			if c.sessionID == sessionID {
				c.sessionID = ""
				c.sessionCWD = ""
			}
			c.mu.Unlock()
		}
		return nil
	}
	if sessionID != "" && sessionCWD == workingDir {
		return nil
	}
	if sessionID != "" {
		c.closeSession(sessionID)
		c.mu.Lock()
		if c.sessionID == sessionID {
			c.sessionID = ""
			c.sessionCWD = ""
		}
		c.mu.Unlock()
	}
	return c.NewSession(ctx, workingDir)
}

// initialize sends the ACP initialize request and waits for the response.
// The read loop goroutine must already be running to deliver the response.
func (c *Client) initialize(ctx context.Context) error {
	initParams := InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: ClientCapabilities{
			FS: &FSCapability{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
			Terminal: false,
		},
		ClientInfo: &ImplementationInfo{
			Name:    "ggcode",
			Title:   "ggcode ACP Host",
			Version: "1.0",
		},
	}

	result, err := c.sendRequest("initialize", initParams, 30*time.Second)
	if err != nil {
		return fmt.Errorf("initialize request: %w", err)
	}

	var resp InitializeResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return fmt.Errorf("parsing initialize response: %w", err)
	}

	c.mu.Lock()
	c.caps = resp.AgentCapabilities
	c.authMethods = resp.AuthMethods
	c.agentInfo = resp.AgentInfo
	c.initialized = true
	c.mu.Unlock()

	return nil
}

// NewSession creates a new session on the agent.
func (c *Client) NewSession(ctx context.Context, cwd string) error {
	if c.usesFactoryProtocol() {
		return c.newFactorySession(ctx, cwd)
	}
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	mcpServers := cloneMCPServers(c.mcpServers)
	c.mu.Unlock()

	params := NewSessionRequest{
		Meta:       buildSessionOptionsMeta(c.def, c.options.SessionOptions),
		CWD:        cwd,
		MCPServers: mcpServers,
	}

	result, err := c.sendRequest("session/new", params, claudeSessionCreateTimeout(c.def))
	if err != nil {
		return fmt.Errorf("session/new for %s: %w", c.def.Def.Name, err)
	}

	var resp NewSessionResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return fmt.Errorf("parsing session/new response: %w", err)
	}

	c.mu.Lock()
	c.sessionID = resp.SessionID
	c.sessionCWD = cwd
	c.sessionModes = cloneSessionModeState(resp.Modes)
	c.sessionConfigOptions = cloneSessionConfigOptions(resp.ConfigOptions)
	c.mu.Unlock()

	debugLogf("created session %s on agent %q", resp.SessionID, c.def.Def.Name)
	return nil
}

// ResumeSession restores a previously created session on the agent.
func (c *Client) ResumeSession(ctx context.Context, sessionID string, cwd string) (*ResumeSessionResponse, error) {
	if c.usesFactoryProtocol() {
		return c.resumeFactorySession(ctx, sessionID, cwd)
	}
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil, fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	mcpServers := cloneMCPServers(c.mcpServers)
	c.mu.Unlock()

	params := ResumeSessionRequest{
		SessionID:  sessionID,
		CWD:        cwd,
		MCPServers: mcpServers,
	}

	result, err := c.sendRequest("session/resume", params, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("session/resume for %s: %w", c.def.Def.Name, err)
	}

	var resp ResumeSessionResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parsing session/resume response: %w", err)
	}

	c.mu.Lock()
	c.sessionID = sessionID
	c.sessionCWD = cwd
	c.sessionModes = cloneSessionModeState(resp.Modes)
	c.sessionConfigOptions = cloneSessionConfigOptions(resp.ConfigOptions)
	c.mu.Unlock()

	return &resp, nil
}

func (c *Client) usesFactoryProtocol() bool {
	return normalizeWireProtocol(c.def.Def.WireProtocol) == WireProtocolFactoryJSONRPC
}

func (c *Client) newFactorySession(ctx context.Context, cwd string) error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	options := cloneSessionOptions(c.options.SessionOptions)
	c.mu.Unlock()
	params := FactoryInitializeSessionRequest{
		MachineID: "default",
		CWD:       cwd,
	}
	if options != nil && strings.TrimSpace(options.Model) != "" {
		params.ModelID = strings.TrimSpace(options.Model)
	}
	result, err := c.sendRequest(droidMethodInitializeSession, params, 30*time.Second)
	if err != nil {
		return fmt.Errorf("%s for %s: %w", droidMethodInitializeSession, c.def.Def.Name, err)
	}
	var resp FactoryInitializeSessionResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return fmt.Errorf("parsing %s response: %w", droidMethodInitializeSession, err)
	}
	c.mu.Lock()
	c.sessionID = resp.SessionID
	c.sessionCWD = cwd
	c.mu.Unlock()
	return nil
}

func (c *Client) resumeFactorySession(ctx context.Context, sessionID string, cwd string) (*ResumeSessionResponse, error) {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil, fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	c.mu.Unlock()
	result, err := c.sendRequest(droidMethodLoadSession, FactoryLoadSessionRequest{SessionID: sessionID}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s for %s: %w", droidMethodLoadSession, c.def.Def.Name, err)
	}
	var resp FactoryLoadSessionResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parsing %s response: %w", droidMethodLoadSession, err)
	}
	if strings.TrimSpace(resp.CWD) == "" {
		resp.CWD = cwd
	}
	c.mu.Lock()
	c.sessionID = sessionID
	c.sessionCWD = resp.CWD
	c.mu.Unlock()
	return &ResumeSessionResponse{}, nil
}

// ListSessions returns the remote session index exposed by the agent.
func (c *Client) ListSessions(ctx context.Context, cursor string, cwd string) (*ListSessionsResponse, error) {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil, fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	c.mu.Unlock()

	params := ListSessionsRequest{
		Cursor: cursor,
		CWD:    cwd,
	}
	result, err := c.sendRequest("session/list", params, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("session/list for %s: %w", c.def.Def.Name, err)
	}

	var resp ListSessionsResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parsing session/list response: %w", err)
	}
	return &resp, nil
}

// SetSessionMode switches the mode of the currently active session.
func (c *Client) SetSessionMode(ctx context.Context, mode SessionModeId) error {
	c.mu.Lock()
	sessionID := c.sessionID
	running := c.running
	c.mu.Unlock()
	if !running {
		return fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	if sessionID == "" {
		return fmt.Errorf("session is not initialized")
	}
	params := SetSessionModeRequest{
		SessionID: sessionID,
		Mode:      mode,
	}
	if _, err := c.sendRequest("session/set_mode", params, 30*time.Second); err != nil {
		return fmt.Errorf("session/set_mode for %s: %w", c.def.Def.Name, err)
	}
	return nil
}

// SetSessionModel switches the model of the currently active session.
func (c *Client) SetSessionModel(ctx context.Context, modelID string) error {
	c.mu.Lock()
	sessionID := c.sessionID
	running := c.running
	c.mu.Unlock()
	if !running {
		return fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	if sessionID == "" {
		return fmt.Errorf("session is not initialized")
	}
	params := SetSessionModelRequest{
		SessionID: sessionID,
		ModelID:   modelID,
	}
	if _, err := c.sendRequest("session/set_model", params, 30*time.Second); err != nil {
		return fmt.Errorf("session/set_model for %s: %w", c.def.Def.Name, err)
	}
	return nil
}

// SetSessionConfigOption updates a config selector for the current session.
func (c *Client) SetSessionConfigOption(ctx context.Context, configID SessionConfigId, value SessionConfigValueId) (*SetSessionConfigOptionResponse, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	running := c.running
	c.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session is not initialized")
	}
	params := SetSessionConfigOptionRequest{
		SessionID: sessionID,
		ConfigID:  configID,
		Value:     value,
	}
	result, err := c.sendRequest("session/set_config", params, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("session/set_config for %s: %w", c.def.Def.Name, err)
	}

	var resp SetSessionConfigOptionResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parsing session/set_config response: %w", err)
	}
	return &resp, nil
}

func cloneMCPServers(servers []MCPServer) []MCPServer {
	if len(servers) == 0 {
		return []MCPServer{}
	}
	cloned := make([]MCPServer, len(servers))
	for i, server := range servers {
		cloned[i] = server
		if len(server.Args) > 0 {
			cloned[i].Args = append([]string(nil), server.Args...)
		}
		if len(server.Env) > 0 {
			cloned[i].Env = append([]EnvVariable(nil), server.Env...)
		}
		if len(server.Headers) > 0 {
			cloned[i].Headers = append([]HTTPHeader(nil), server.Headers...)
		}
	}
	return cloned
}

func cloneSessionModeState(state *SessionModeState) *SessionModeState {
	if state == nil {
		return nil
	}
	cloned := *state
	if len(state.Modes) > 0 {
		cloned.Modes = append([]SessionMode(nil), state.Modes...)
	}
	return &cloned
}

func cloneSessionConfigOptions(options []SessionConfigOption) []SessionConfigOption {
	if len(options) == 0 {
		return nil
	}
	cloned := make([]SessionConfigOption, len(options))
	for i, option := range options {
		cloned[i] = option
		if len(option.Options) > 0 {
			cloned[i].Options = append(SessionConfigSelectOptions(nil), option.Options...)
		}
	}
	return cloned
}

func cloneAvailableCommands(commands []AvailableCommand) []AvailableCommand {
	if len(commands) == 0 {
		return nil
	}
	cloned := make([]AvailableCommand, len(commands))
	copy(cloned, commands)
	return cloned
}

func commandBaseName(command string) string {
	value := strings.TrimSpace(command)
	if value == "" {
		return ""
	}
	base := filepath.Base(value)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return strings.ToLower(base)
}

func isGeminiAcpCommand(command string, args []string) bool {
	return commandBaseName(command) == "gemini" && (hasCommandFlag(args, "--acp") || hasCommandFlag(args, "--experimental-acp"))
}

func resolveGeminiCommandArgs(command string, args []string) []string {
	return resolveGeminiCommandArgsWithReader(command, args, readCommandOutput)
}

func resolveGeminiCommandArgsWithReader(
	command string,
	args []string,
	read func(string, []string, time.Duration) (string, error),
) []string {
	if !isGeminiAcpCommand(command, args) || !hasCommandFlag(args, "--acp") {
		return args
	}
	output, err := read(command, []string{"--version"}, defaultCopilotHelpTimeout)
	if err != nil {
		return args
	}
	version := extractSemanticVersion(output)
	if version == nil || compareSemanticVersion(*version, [3]int{0, 33, 0}) >= 0 {
		return args
	}
	rewritten := append([]string(nil), args...)
	for i, arg := range rewritten {
		if arg == "--acp" {
			rewritten[i] = "--experimental-acp"
		}
	}
	return rewritten
}

func ensureCopilotAcpSupport(command string, args []string) error {
	return ensureCopilotAcpSupportWithReader(command, args, readCommandOutput)
}

func ensureCopilotAcpSupportWithReader(
	command string,
	args []string,
	read func(string, []string, time.Duration) (string, error),
) error {
	if commandBaseName(command) != "copilot" || !hasCommandFlag(args, "--acp") {
		return nil
	}
	output, err := read(command, []string{"--help"}, defaultCopilotHelpTimeout)
	if err != nil {
		return nil
	}
	if strings.Contains(output, "--acp") {
		return nil
	}
	return fmt.Errorf("github copilot CLI ACP stdio mode is not available in the installed copilot binary; upgrade Copilot CLI to a release with --acp --stdio support")
}

func readCommandOutput(command string, args []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", ctx.Err()
	}
	return string(output), err
}

func extractSemanticVersion(output string) *[3]int {
	matches := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(output)
	if len(matches) != 4 {
		return nil
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return nil
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return nil
	}
	patch, err := strconv.Atoi(matches[3])
	if err != nil {
		return nil
	}
	version := [3]int{major, minor, patch}
	return &version
}

func compareSemanticVersion(left, right [3]int) int {
	for i := 0; i < len(left); i++ {
		if left[i] == right[i] {
			continue
		}
		return left[i] - right[i]
	}
	return 0
}

func claudeSessionCreateTimeout(agent DiscoveredAgent) time.Duration {
	if agent.Def.Name != "claude" {
		return 30 * time.Second
	}
	for _, key := range []string{
		"GGCODE_ACP_CLAUDE_SESSION_CREATE_TIMEOUT_MS",
		"ACPX_CLAUDE_ACP_SESSION_CREATE_TIMEOUT_MS",
	} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			continue
		}
		return time.Duration(value) * time.Millisecond
	}
	return defaultClaudeSessionTimeout
}

func resolvedAgentArgs(agent DiscoveredAgent, options *SessionOptions) []string {
	args := append([]string(nil), agent.Args...)
	if options == nil {
		return args
	}
	if agent.Def.Name == "qoder" {
		args = appendQoderSessionArgs(args, options)
	}
	return args
}

func appendQoderSessionArgs(args []string, options *SessionOptions) []string {
	if options == nil {
		return args
	}
	if options.MaxTurns > 0 && !hasCommandFlag(args, "--max-turns") {
		args = append(args, fmt.Sprintf("--max-turns=%d", options.MaxTurns))
	}
	if len(options.AllowedTools) > 0 && !hasCommandFlag(args, "--allowed-tools") && !hasCommandFlag(args, "--disallowed-tools") {
		normalized := make([]string, 0, len(options.AllowedTools))
		for _, tool := range options.AllowedTools {
			switch strings.ToLower(strings.TrimSpace(tool)) {
			case "bash", "glob", "grep", "ls", "read", "write":
				normalized = append(normalized, strings.ToUpper(strings.TrimSpace(tool)))
			default:
				if strings.TrimSpace(tool) != "" {
					normalized = append(normalized, strings.TrimSpace(tool))
				}
			}
		}
		if len(normalized) > 0 {
			args = append(args, "--allowed-tools="+strings.Join(normalized, ","))
		}
	}
	return args
}

func hasCommandFlag(args []string, flagName string) bool {
	for _, arg := range args {
		if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
			return true
		}
	}
	return false
}

func buildSessionOptionsMeta(agent DiscoveredAgent, options *SessionOptions) json.RawMessage {
	if options == nil {
		return nil
	}
	meta := make(map[string]interface{})
	if agent.Def.Name == "claude" {
		claudeOptions := make(map[string]interface{})
		if value := strings.TrimSpace(options.Model); value != "" {
			claudeOptions["model"] = value
		}
		if len(options.AllowedTools) > 0 {
			claudeOptions["allowedTools"] = append([]string(nil), options.AllowedTools...)
		}
		if options.MaxTurns > 0 {
			claudeOptions["maxTurns"] = options.MaxTurns
		}
		if len(claudeOptions) > 0 {
			meta["claudeCode"] = map[string]interface{}{"options": claudeOptions}
		}
		assignSystemPrompt(meta, options.SystemPrompt)
	}
	if len(meta) == 0 {
		return nil
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return payload
}

func assignSystemPrompt(target map[string]interface{}, value interface{}) {
	switch prompt := value.(type) {
	case string:
		if strings.TrimSpace(prompt) != "" {
			target["systemPrompt"] = prompt
		}
	case SystemPromptOption:
		if strings.TrimSpace(prompt.Append) != "" {
			target["systemPrompt"] = map[string]string{"append": prompt.Append}
		}
	case *SystemPromptOption:
		if prompt != nil && strings.TrimSpace(prompt.Append) != "" {
			target["systemPrompt"] = map[string]string{"append": prompt.Append}
		}
	case map[string]interface{}:
		if appendValue, ok := prompt["append"].(string); ok && strings.TrimSpace(appendValue) != "" {
			target["systemPrompt"] = map[string]string{"append": appendValue}
		}
	}
}

// promptInternal sends a prompt and collects the full response.
// Blocks until the agent completes (end_turn, error, etc.).
func (c *Client) promptInternal(
	ctx context.Context,
	prompt string,
	onEvent func(PromptEvent),
) (*PromptResult, error) {
	c.execMu.Lock()
	defer c.execMu.Unlock()

	c.mu.Lock()
	sessionID := c.sessionID
	if !c.running {
		c.mu.Unlock()
		return nil, fmt.Errorf("agent %q is not running", c.def.Def.Name)
	}
	c.mu.Unlock()

	// Reset prompt state
	c.promptMu.Lock()
	c.promptText.Reset()
	c.promptTools = nil
	c.promptToolMeta = make(map[string]*promptToolState)
	c.activePromptID = sessionID
	c.promptDone = make(chan PromptResponse, 1)
	c.promptActivity = make(chan struct{}, 1)
	c.promptOnEvent = onEvent
	c.droidSeenNonIdle = false
	c.droidLastAssistantText = ""
	c.droidLastFallbackText = ""
	promptDone := c.promptDone
	promptActivity := c.promptActivity
	promptIdleTime := c.promptIdleTime
	c.promptMu.Unlock()
	c.activity.Reset()
	factoryProtocol := c.usesFactoryProtocol()
	promptMethod := "session/prompt"
	var promptReq interface{} = PromptRequest{
		SessionID: sessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: prompt}},
	}
	if factoryProtocol {
		promptMethod = droidMethodAddUserMessage
		promptReq = FactoryAddUserMessageRequest{Text: prompt}
		c.recordActivity("sent %s prompt_len=%d", promptMethod, len(prompt))
	} else {
		c.recordActivity("sent session/prompt prompt_len=%d", len(prompt))
	}
	type promptRequestResult struct {
		response PromptResponse
		err      error
	}
	promptRespCh := make(chan promptRequestResult, 1)
	go func() {
		result, err := c.sendRequest(promptMethod, promptReq, c.promptReqTime)
		if err != nil {
			promptRespCh <- promptRequestResult{err: err}
			return
		}
		if factoryProtocol {
			c.recordActivity("recv %s response", promptMethod)
			promptRespCh <- promptRequestResult{}
			return
		}
		var resp PromptResponse
		if len(strings.TrimSpace(string(result))) > 0 && string(result) != "{}" && string(result) != "null" {
			if err := json.Unmarshal(result, &resp); err != nil {
				promptRespCh <- promptRequestResult{err: fmt.Errorf("parsing %s response: %w", promptMethod, err)}
				return
			}
		}
		stop := "ack"
		if resp.StopReason != "" {
			stop = summarizeStopReason(resp.StopReason)
		}
		c.recordActivity("recv session/prompt response stop=%s", stop)
		promptRespCh <- promptRequestResult{response: resp}
	}()

	idleTimer := time.NewTimer(promptIdleTime)
	defer idleTimer.Stop()

	for {
		select {
		case resp := <-promptDone:
			c.promptMu.Lock()
			pr := &PromptResult{
				Text:       c.promptText.String(),
				StopReason: resp.StopReason,
				ToolCalls:  c.promptTools,
			}
			c.clearPromptStateLocked()
			c.promptMu.Unlock()
			return pr, nil
		case rpc := <-promptRespCh:
			if rpc.err != nil {
				c.promptMu.Lock()
				c.clearPromptStateLocked()
				c.promptMu.Unlock()
				return nil, rpc.err
			}
			if rpc.response.StopReason != "" {
				c.promptMu.Lock()
				pr := &PromptResult{
					Text:       c.promptText.String(),
					StopReason: rpc.response.StopReason,
					ToolCalls:  c.promptTools,
				}
				c.clearPromptStateLocked()
				c.promptMu.Unlock()
				return pr, nil
			}
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(promptIdleTime)
		case <-promptActivity:
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(promptIdleTime)
		case <-ctx.Done():
			c.sendCancel(sessionID)
			c.promptMu.Lock()
			c.clearPromptStateLocked()
			c.promptMu.Unlock()
			return nil, ctx.Err()
		case <-idleTimer.C:
			activity := c.activity.Snapshot()
			c.sendCancel(sessionID)
			c.promptMu.Lock()
			c.clearPromptStateLocked()
			c.promptMu.Unlock()
			return nil, c.annotateTimeoutError(fmt.Errorf("timeout waiting for agent prompt completion after %s", promptIdleTime), activity)
		case <-c.done:
			activity := c.activity.Snapshot()
			c.promptMu.Lock()
			c.clearPromptStateLocked()
			c.promptMu.Unlock()
			if readErr := c.getReadErr(); readErr != nil {
				return nil, c.annotatePromptError(fmt.Errorf("agent %q transport failed: %w", c.def.Def.Name, readErr), activity)
			}
			return nil, c.annotatePromptError(fmt.Errorf("agent %q process exited unexpectedly", c.def.Def.Name), activity)
		}
	}
}

func (c *Client) shutdown(closeSession bool) error {
	c.mu.Lock()
	sessionID := c.sessionID
	running := c.running
	cancelRead := c.cancelRead
	cancelProc := c.cancelProc
	cmd := c.cmd
	stdin := c.stdin
	transport := c.transport
	c.mu.Unlock()

	if closeSession && running && sessionID != "" {
		c.closeSession(sessionID)
	}

	if transport != nil {
		_ = transport.CloseWriter()
	} else if stdin != nil {
		_ = stdin.Close()
	}

	if cancelRead != nil {
		cancelRead()
	}

	if cancelProc != nil {
		cancelProc()
	}

	if cmd != nil && cmd.Process != nil {
		_ = killACPProcess(cmd)
		_ = cmd.Wait()
	}

	c.mu.Lock()
	c.running = false
	c.initialized = false
	c.cancelRead = nil
	c.cancelProc = nil
	c.cmd = nil
	c.stdin = nil
	c.transport = nil
	c.readErr = nil
	c.sessionID = ""
	c.sessionCWD = ""
	c.sessionTitle = ""
	c.sessionModes = nil
	c.sessionConfigOptions = nil
	c.availableCommands = nil
	c.mu.Unlock()

	return nil
}

// Disconnect tears down the local agent process without closing the remote session.
func (c *Client) Disconnect() error {
	return c.shutdown(false)
}

// Close sends session/close (if session exists) and kills the process.
func (c *Client) Close() error {
	return c.shutdown(true)
}

// CancelActivePrompt requests cancellation of the current session prompt.
func (c *Client) CancelActivePrompt() {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	if sessionID == "" {
		return
	}
	c.sendCancel(sessionID)
}

// sendRequest sends a JSON-RPC request via the transport and waits for response.
func (c *Client) sendRequest(method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	result, err := c.transport.SendRequest(method, params, timeout)
	if err == nil {
		return result, nil
	}
	return nil, c.annotateTimeoutError(err, c.activity.Snapshot())
}

// sendCancel sends a session/cancel notification.
func (c *Client) sendCancel(sessionID string) {
	if c.usesFactoryProtocol() {
		_, _ = c.sendRequest(droidMethodInterruptSession, FactoryInterruptSessionRequest{}, defaultCloseSessionTimeout)
		return
	}
	_ = c.writeNotification("session/cancel", CancelNotification{
		SessionID: sessionID,
	})
}

func (c *Client) closeSession(sessionID string) {
	if c.transport == nil || sessionID == "" {
		return
	}
	if c.usesFactoryProtocol() {
		_, _ = c.sendRequest(droidMethodCloseSession, FactoryCloseSessionRequest{Reason: "other"}, defaultCloseSessionTimeout)
		return
	}
	_, _ = c.sendRequest("session/close", CloseSessionRequest{SessionID: sessionID}, defaultCloseSessionTimeout)
}

func (c *Client) annotateTimeoutError(err error, activity string) error {
	if err == nil {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, "timeout waiting for client response") &&
		!strings.Contains(msg, "timeout waiting for agent prompt completion") {
		return err
	}
	if activity != "" {
		err = fmt.Errorf("%w\nRecent ACP activity:\n%s", err, activity)
	}
	stderr := c.stderrTail.Snapshot()
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w\nRecent agent stderr:\n%s", err, stderr)
}

func (c *Client) annotatePromptError(err error, activity string) error {
	if err == nil {
		return nil
	}
	if activity != "" {
		err = fmt.Errorf("%w\nRecent ACP activity:\n%s", err, activity)
	}
	stderr := c.stderrTail.Snapshot()
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w\nRecent agent stderr:\n%s", err, stderr)
}

func (c *Client) setReadErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readErr = err
}

func (c *Client) getReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// ---------- read loop ----------

func (c *Client) readLoop(ctx context.Context) {
	defer close(c.done)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		req, resp, err := c.transport.ReadAnyMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				debugLogf("agent %q process EOF", c.def.Def.Name)
				return
			}
			c.recordActivity("transport read error=%s", summarizeError(err))
			c.setReadErr(err)
			debugLogf("agent %q read error: %v", c.def.Def.Name, err)
			return
		}

		// Response to our pending request
		if resp != nil {
			c.transport.DeliverResponse(resp)
			continue
		}

		// Request/notification FROM the agent
		if req != nil {
			c.handleAgentRequest(ctx, req)
		}
	}
}

func (c *Client) handleAgentRequest(ctx context.Context, req *JSONRPCRequest) {
	switch req.Method {
	case "session/update":
		c.notePromptActivity()
		c.handleSessionUpdate(req)

	case droidMethodSessionNotification:
		c.notePromptActivity()
		c.handleFactorySessionNotification(req)

	case "fs/read_text_file":
		c.notePromptActivity()
		c.handleFSRead(ctx, req)

	case "fs/write_text_file":
		c.notePromptActivity()
		c.handleFSWrite(ctx, req)

	case "session/prompt_complete":
		c.handlePromptComplete(req)

	case "session/info_update":
		c.handleSessionInfoUpdate(req)

	case "session/current_mode_update":
		c.handleCurrentModeUpdate(req)

	case "session/config_options_update":
		c.handleConfigOptionUpdate(req)

	case "session/available_commands_update":
		c.handleAvailableCommandsUpdate(req)

	case "session/request_permission":
		c.notePromptActivity()
		c.handlePermission(ctx, req)

	case droidMethodRequestPermission:
		c.notePromptActivity()
		c.handleFactoryPermission(ctx, req)

	case droidMethodAskUser:
		c.notePromptActivity()
		c.handleFactoryAskUser(req)

	case "terminal/create":
		c.notePromptActivity()
		c.handleTerminalCreate(req)

	case "terminal/output":
		c.notePromptActivity()
		c.handleTerminalOutput(req)

	case "terminal/wait_for_exit":
		c.notePromptActivity()
		c.handleTerminalWaitForExit(req)

	case "terminal/kill":
		c.notePromptActivity()
		c.handleTerminalKill(req)

	case "terminal/release":
		c.notePromptActivity()
		c.handleTerminalRelease(req)

	default:
		c.recordActivity("recv unsupported method=%s", req.Method)
		if req.ID != nil {
			_ = c.writeError(req.ID, -32601, "host does not support: "+req.Method)
		}
	}
}

func (c *Client) clearPromptStateLocked() {
	c.activePromptID = ""
	c.promptDone = nil
	c.promptActivity = nil
	c.promptOnEvent = nil
	c.promptToolMeta = nil
	c.droidSeenNonIdle = false
	c.droidLastAssistantText = ""
	c.droidLastFallbackText = ""
}

func (c *Client) notePromptActivity() {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	if c.activePromptID == "" || c.promptActivity == nil {
		return
	}
	select {
	case c.promptActivity <- struct{}{}:
	default:
	}
}

func (c *Client) recordActivity(format string, args ...interface{}) {
	c.activity.Add(fmt.Sprintf(format, args...))
}

func (c *Client) transportSnapshot() *Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.transport
}

func (c *Client) writeNotification(method string, params interface{}) error {
	transport := c.transportSnapshot()
	if transport == nil {
		return nil
	}
	return transport.WriteNotification(method, params)
}

func (c *Client) writeResponse(id interface{}, result interface{}) error {
	transport := c.transportSnapshot()
	if transport == nil {
		return nil
	}
	return transport.WriteResponse(id, result)
}

func (c *Client) writeError(id interface{}, code int, message string) error {
	transport := c.transportSnapshot()
	if transport == nil {
		return nil
	}
	return transport.WriteError(id, code, message)
}

// ---------- session/update ----------

func (c *Client) handleSessionUpdate(req *JSONRPCRequest) {
	var notif SessionNotification
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("session/update parse_error=%s", err)
		debugLogf("parse session/update: %v", err)
		return
	}

	c.recordActivity("%s", summarizeSessionUpdateActivity(notif.Update))

	c.promptMu.Lock()
	var emitted []PromptEvent

	switch notif.Update.Type {
	case UpdateAgentMessageChunk:
		if block, ok := contentBlockFromAny(notif.Update.Content); ok {
			switch block.Type {
			case "", "text":
				c.promptText.WriteString(block.Text)
				if block.Text != "" {
					emitted = append(emitted, PromptEvent{
						Type: PromptEventText,
						Text: block.Text,
					})
				}
			}
		}

	case UpdateToolCall:
		state := c.mergePromptToolStateLocked(notif.Update)
		c.updatePromptToolSummaryLocked(state)
		if !state.startedEmitted {
			state.startedEmitted = true
			emitted = append(emitted, c.promptToolCallEvent(state))
		}

	case UpdateToolCallUpdate:
		state := c.mergePromptToolStateLocked(notif.Update)
		c.updatePromptToolSummaryLocked(state)
		if !state.startedEmitted {
			state.startedEmitted = true
			emitted = append(emitted, c.promptToolCallEvent(state))
		}
		if notif.Update.Status == ToolCallStatusCompleted || notif.Update.Status == ToolCallStatusFailed {
			emitted = append(emitted, c.promptToolResultEvent(state, notif.Update))
		}
	}
	onEvent := c.promptOnEvent
	c.promptMu.Unlock()
	for _, event := range emitted {
		if onEvent != nil {
			onEvent(event)
		}
	}
}

func (c *Client) handleFactorySessionNotification(req *JSONRPCRequest) {
	var notif FactorySessionNotificationParams
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("droid.session_notification parse_error=%s", err)
		debugLogf("parse droid.session_notification: %v", err)
		return
	}
	c.recordActivity("%s", summarizeFactorySessionNotificationActivity(notif.Notification))

	var titleUpdate string
	c.promptMu.Lock()
	var emitted []PromptEvent

	switch notif.Notification.Type {
	case droidNotificationAssistantTextDelta:
		c.markFactoryPromptProgressLocked()
		c.promptText.WriteString(notif.Notification.TextDelta)
		if notif.Notification.TextDelta != "" {
			emitted = append(emitted, PromptEvent{
				Type: PromptEventText,
				Text: notif.Notification.TextDelta,
			})
		}
	case droidNotificationToolCall:
		c.markFactoryPromptProgressLocked()
		if notif.Notification.ToolUse != nil {
			state := c.mergeFactoryPromptToolUseLocked(*notif.Notification.ToolUse)
			c.updatePromptToolSummaryLocked(state)
			if !state.startedEmitted {
				state.startedEmitted = true
				emitted = append(emitted, c.promptToolCallEvent(state))
			}
		}
	case droidNotificationToolResult:
		c.markFactoryPromptProgressLocked()
		state := c.mergeFactoryPromptToolResultLocked(notif.Notification)
		c.updatePromptToolSummaryLocked(state)
		emitted = append(emitted, c.promptToolResultEvent(state, SessionUpdate{
			Status: state.status,
		}))
	case droidNotificationCreateMessage:
		c.markFactoryPromptProgressLocked()
		if notif.Notification.Message != nil {
			text := extractFactoryMessageText(notif.Notification.Message)
			switch notif.Notification.Message.Role {
			case "assistant":
				c.droidLastAssistantText = text
			case "system":
				if strings.TrimSpace(text) != "" {
					c.droidLastFallbackText = text
				}
			}
		}
	case droidNotificationError:
		c.markFactoryPromptProgressLocked()
		if strings.TrimSpace(notif.Notification.MessageText) != "" {
			c.droidLastFallbackText = notif.Notification.MessageText
		}
	case droidNotificationWorkingStateChanged:
		if c.activePromptID != "" && notif.Notification.NewState != "" && notif.Notification.NewState != droidWorkingStateIdle {
			c.droidSeenNonIdle = true
		}
		if c.activePromptID != "" && c.promptDone != nil && notif.Notification.NewState == droidWorkingStateIdle && c.droidSeenNonIdle {
			if c.promptText.Len() == 0 && strings.TrimSpace(c.droidLastAssistantText) != "" {
				c.promptText.WriteString(c.droidLastAssistantText)
			} else if c.promptText.Len() == 0 && strings.TrimSpace(c.droidLastFallbackText) != "" {
				c.promptText.WriteString(c.droidLastFallbackText)
			}
			select {
			case c.promptDone <- PromptResponse{StopReason: StopReasonEndTurn}:
			default:
			}
		}
	case droidNotificationSessionTitleUpdated:
		titleUpdate = strings.TrimSpace(notif.Notification.Title)
	}

	onEvent := c.promptOnEvent
	c.promptMu.Unlock()
	if titleUpdate != "" {
		c.mu.Lock()
		c.sessionTitle = titleUpdate
		c.mu.Unlock()
	}
	for _, event := range emitted {
		if onEvent != nil {
			onEvent(event)
		}
	}
}

func contentBlockFromAny(content interface{}) (ContentBlock, bool) {
	if content == nil {
		return ContentBlock{}, false
	}
	if block, ok := content.(*ContentBlock); ok && block != nil {
		return *block, true
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return ContentBlock{}, false
	}
	var block ContentBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		return ContentBlock{}, false
	}
	return block, true
}

func rawMessageString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return ""
	}
	return text
}

func toolPayloadDisplayString(raw json.RawMessage) string {
	text := rawMessageString(raw)
	switch text {
	case "", "null", "{}", "[]":
		return ""
	default:
		return text
	}
}

func summarizeSessionUpdateActivity(update SessionUpdate) string {
	switch update.Type {
	case UpdateAgentMessageChunk:
		if block, ok := contentBlockFromAny(update.Content); ok {
			switch block.Type {
			case "", "text":
				return fmt.Sprintf("session/update text_chunk len=%d", len(block.Text))
			default:
				return fmt.Sprintf("session/update content type=%s", summarizeText(block.Type))
			}
		}
		return "session/update agent_message_chunk"
	case UpdateToolCall:
		return fmt.Sprintf("session/update tool_call id=%s title=%s kind=%s", summarizeText(update.ToolCallID), summarizeText(update.Title), summarizeText(string(update.Kind)))
	case UpdateToolCallUpdate:
		result := summarizeRawJSONText(update.RawOutput)
		if result != "" {
			return fmt.Sprintf("session/update tool_call_update id=%s title=%s status=%s result=%s", summarizeText(update.ToolCallID), summarizeText(update.Title), summarizeText(string(update.Status)), summarizeText(result))
		}
		return fmt.Sprintf("session/update tool_call_update id=%s title=%s status=%s", summarizeText(update.ToolCallID), summarizeText(update.Title), summarizeText(string(update.Status)))
	default:
		return fmt.Sprintf("session/update type=%s", summarizeText(string(update.Type)))
	}
}

func summarizePermissionActivity(params RequestPermissionRequest) string {
	title := ""
	kind := ""
	if params.ToolCall != nil {
		title = params.ToolCall.Title
		kind = string(params.ToolCall.Kind)
	}
	options := summarizePermissionOptions(params.Options)
	if title != "" || kind != "" {
		return fmt.Sprintf("title=%s kind=%s options=%d %s", summarizeText(title), summarizeText(kind), len(params.Options), options)
	}
	return fmt.Sprintf("options=%d %s", len(params.Options), options)
}

func summarizePermissionResponseActivity(resp RequestPermissionResponse) string {
	if resp.Outcome.SelectedOption != nil {
		return fmt.Sprintf("outcome=%s option=%s", summarizeText(resp.Outcome.Outcome), summarizeText(resp.Outcome.SelectedOption.OptionID))
	}
	return fmt.Sprintf("outcome=%s", summarizeText(resp.Outcome.Outcome))
}

func summarizeRawJSONText(raw json.RawMessage) string {
	text := rawMessageString(raw)
	if text == "" {
		return ""
	}
	var quoted string
	if err := json.Unmarshal(raw, &quoted); err == nil {
		return quoted
	}
	return text
}

func (c *Client) mergePromptToolStateLocked(update SessionUpdate) *promptToolState {
	if c.promptToolMeta == nil {
		c.promptToolMeta = make(map[string]*promptToolState)
	}
	toolID := strings.TrimSpace(update.ToolCallID)
	state := c.promptToolMeta[toolID]
	if state == nil {
		state = &promptToolState{toolID: toolID}
		c.promptToolMeta[toolID] = state
	}
	if update.hasTitle {
		state.title = strings.TrimSpace(update.Title)
	}
	if update.hasKind {
		state.kind = update.Kind
	}
	if update.hasStatus {
		state.status = update.Status
	}
	if update.hasRawInput {
		state.rawInput = append(json.RawMessage(nil), update.RawInput...)
	}
	if update.hasRawOutput {
		state.rawOutput = append(json.RawMessage(nil), update.RawOutput...)
	}
	if update.hasContent {
		state.content = update.Content
	}
	if update.hasLocations {
		if update.Locations == nil {
			state.locations = nil
		} else {
			state.locations = append([]ToolCallLocation(nil), update.Locations...)
		}
	}
	if state.toolName == "" {
		if toolName := strings.TrimSpace(acpToolName(update.Title, update.Kind)); toolName != "" {
			state.toolName = toolName
		}
	}
	if state.toolName == "" {
		state.toolName = strings.TrimSpace(acpToolName(state.title, state.kind))
	}
	return state
}

func (c *Client) updatePromptToolSummaryLocked(state *promptToolState) {
	if state == nil {
		return
	}
	title := promptToolStateTitle(state)
	status := string(state.status)
	for i := range c.promptTools {
		if c.promptTools[i].Name != state.toolID {
			continue
		}
		if title != "" {
			c.promptTools[i].Title = title
		}
		if status != "" {
			c.promptTools[i].Status = status
		}
		return
	}
	c.promptTools = append(c.promptTools, ToolCallSummary{
		Name:   state.toolID,
		Title:  title,
		Status: status,
	})
}

func (c *Client) promptToolCallEvent(state *promptToolState) PromptEvent {
	return PromptEvent{
		Type:      PromptEventToolCall,
		ToolID:    state.toolID,
		ToolName:  promptToolStateName(state),
		ToolTitle: promptToolStateTitle(state),
		ToolArgs:  toolPayloadDisplayString(state.rawInput),
	}
}

func (c *Client) promptToolResultEvent(state *promptToolState, update SessionUpdate) PromptEvent {
	return PromptEvent{
		Type:      PromptEventToolResult,
		ToolID:    state.toolID,
		ToolName:  promptToolStateName(state),
		ToolTitle: promptToolStateTitle(state),
		ToolArgs:  toolPayloadDisplayString(state.rawInput),
		Result:    promptToolResultText(state, update),
		IsError:   state.status == ToolCallStatusFailed || update.Status == ToolCallStatusFailed,
	}
}

func promptToolStateName(state *promptToolState) string {
	if state == nil {
		return "tool"
	}
	if value := strings.TrimSpace(state.toolName); value != "" {
		return value
	}
	if value := strings.TrimSpace(acpToolName(state.title, state.kind)); value != "" {
		return value
	}
	return "tool"
}

func promptToolStateTitle(state *promptToolState) string {
	if state == nil {
		return "tool"
	}
	if value := strings.TrimSpace(state.title); value != "" {
		return value
	}
	return promptToolStateName(state)
}

func promptToolResultText(state *promptToolState, update SessionUpdate) string {
	if out := toolCallUpdateResult(update); out != "" {
		return out
	}
	if state == nil {
		return ""
	}
	if out := summarizeRawJSONText(state.rawOutput); out != "" {
		return out
	}
	if entries, ok := toolCallEntriesFromAny(state.content); ok {
		parts := make([]string, 0, len(entries))
		for _, entry := range entries {
			switch entry.Type {
			case "content":
				if entry.Content != nil {
					if text := strings.TrimSpace(entry.Content.Text); text != "" {
						parts = append(parts, text)
					}
				}
			case "diff":
				if path := strings.TrimSpace(entry.Path); path != "" {
					parts = append(parts, "updated "+path)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if state.status != "" {
		return string(state.status)
	}
	return ""
}

func summarizeStopReason(reason StopReason) string {
	if reason == "" {
		return "unknown"
	}
	return string(reason)
}

func summarizePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "-"
	}
	return summarizeText(path)
}

func summarizeError(err error) string {
	if err == nil {
		return "-"
	}
	return summarizeText(err.Error())
}

func summarizeText(text string) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if text == "" {
		return "-"
	}
	return text
}

func summarizeFactorySessionNotificationActivity(payload FactorySessionNotificationPayload) string {
	switch payload.Type {
	case droidNotificationAssistantTextDelta:
		return fmt.Sprintf("droid.session_notification assistant_text_delta len=%d", len(payload.TextDelta))
	case droidNotificationToolCall:
		if payload.ToolUse != nil {
			return fmt.Sprintf("droid.session_notification tool_call id=%s title=%s", summarizeText(payload.ToolUse.ID), summarizeText(payload.ToolUse.Name))
		}
		return "droid.session_notification tool_call"
	case droidNotificationToolResult:
		result := summarizeText(rawJSONValue(payload.Content))
		if result != "-" {
			return fmt.Sprintf("droid.session_notification tool_result id=%s result=%s", summarizeText(payload.ToolUseID), result)
		}
		return fmt.Sprintf("droid.session_notification tool_result id=%s", summarizeText(payload.ToolUseID))
	case droidNotificationWorkingStateChanged:
		return fmt.Sprintf("droid.session_notification working_state state=%s", summarizeText(payload.NewState))
	case droidNotificationCreateMessage:
		role := "-"
		if payload.Message != nil {
			role = summarizeText(payload.Message.Role)
		}
		return fmt.Sprintf("droid.session_notification create_message role=%s", role)
	case droidNotificationSessionTitleUpdated:
		return fmt.Sprintf("droid.session_notification title=%s", summarizeText(payload.Title))
	default:
		return fmt.Sprintf("droid.session_notification type=%s", summarizeText(payload.Type))
	}
}

func summarizePermissionOptions(options []PermissionOption) string {
	if len(options) == 0 {
		return "option_ids=-"
	}
	parts := make([]string, 0, len(options))
	for _, option := range options {
		part := summarizeText(option.OptionID)
		if option.Kind != "" {
			part += ":" + summarizeText(string(option.Kind))
		}
		parts = append(parts, part)
	}
	return "option_ids=" + strings.Join(parts, ",")
}

func acpToolName(title string, kind ToolKind) string {
	if strings.TrimSpace(title) != "" {
		return strings.TrimSpace(title)
	}
	if strings.TrimSpace(string(kind)) != "" {
		return strings.TrimSpace(string(kind))
	}
	return "tool"
}

func toolCallUpdateResult(update SessionUpdate) string {
	if out := rawMessageString(update.RawOutput); out != "" {
		return out
	}
	if entries, ok := toolCallEntriesFromAny(update.Content); ok {
		parts := make([]string, 0, len(entries))
		for _, entry := range entries {
			switch entry.Type {
			case "content":
				if entry.Content != nil {
					if text := strings.TrimSpace(entry.Content.Text); text != "" {
						parts = append(parts, text)
					}
				}
			case "diff":
				if path := strings.TrimSpace(entry.Path); path != "" {
					parts = append(parts, "updated "+path)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if update.Status != "" {
		return string(update.Status)
	}
	return ""
}

func toolCallEntriesFromAny(content interface{}) ([]ToolCallContentEntry, bool) {
	if content == nil {
		return nil, false
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, false
	}
	var entries []ToolCallContentEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	return entries, true
}

func (c *Client) handlePromptComplete(req *JSONRPCRequest) {
	var notif PromptCompleteNotification
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("session/prompt_complete parse_error=%s", err)
		debugLogf("parse session/prompt_complete: %v", err)
		return
	}
	c.recordActivity("recv session/prompt_complete stop=%s", summarizeStopReason(notif.Response.StopReason))

	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	if c.activePromptID == "" || notif.SessionID != c.activePromptID || c.promptDone == nil {
		return
	}
	select {
	case c.promptDone <- notif.Response:
	default:
	}
}

func (c *Client) handleSessionInfoUpdate(req *JSONRPCRequest) {
	var notif SessionInfoUpdate
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("session/info_update parse_error=%s", err)
		return
	}
	c.recordActivity("recv session/info_update title=%s", summarizeText(notif.Title))
	c.mu.Lock()
	if c.sessionID == "" || notif.SessionID == c.sessionID {
		c.sessionTitle = notif.Title
	}
	c.mu.Unlock()
}

func (c *Client) handleCurrentModeUpdate(req *JSONRPCRequest) {
	var notif CurrentModeUpdate
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("session/current_mode_update parse_error=%s", err)
		return
	}
	c.recordActivity("recv session/current_mode_update mode=%s", summarizeText(string(notif.Mode)))
	c.mu.Lock()
	if c.sessionID == "" || notif.SessionID == c.sessionID {
		if c.sessionModes == nil {
			c.sessionModes = &SessionModeState{}
		}
		c.sessionModes.Current = notif.Mode
	}
	c.mu.Unlock()
}

func (c *Client) handleConfigOptionUpdate(req *JSONRPCRequest) {
	var notif ConfigOptionUpdate
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("session/config_options_update parse_error=%s", err)
		return
	}
	c.recordActivity("recv session/config_options_update options=%d", len(notif.ConfigOptions))
	c.mu.Lock()
	if c.sessionID == "" || notif.SessionID == c.sessionID {
		c.sessionConfigOptions = cloneSessionConfigOptions(notif.ConfigOptions)
	}
	c.mu.Unlock()
}

func (c *Client) handleAvailableCommandsUpdate(req *JSONRPCRequest) {
	var notif AvailableCommandsUpdate
	if err := json.Unmarshal(req.Params, &notif); err != nil {
		c.recordActivity("session/available_commands_update parse_error=%s", err)
		return
	}
	c.recordActivity("recv session/available_commands_update commands=%d", len(notif.AvailableCommands))
	c.mu.Lock()
	if c.sessionID == "" || notif.SessionID == c.sessionID {
		c.availableCommands = cloneAvailableCommands(notif.AvailableCommands)
	}
	c.mu.Unlock()
}

// ---------- FS operations ----------

func (c *Client) handleFSRead(ctx context.Context, req *JSONRPCRequest) {
	var params ReadTextFileRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("fs/read_text_file parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv fs/read_text_file path=%s", summarizePath(params.Path))

	absPath, err := c.resolvePath(params.Path)
	if err != nil {
		c.recordActivity("fs/read_text_file error=%s", summarizeError(err))
		_ = c.writeError(req.ID, -32000, err.Error())
		return
	}
	if err := c.authorizeFileTool(ctx, "read_file", absPath, req.Params); err != nil {
		c.recordActivity("fs/read_text_file denied path=%s error=%s", summarizePath(absPath), summarizeError(err))
		_ = c.writeError(req.ID, -32000, err.Error())
		return
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		c.recordActivity("fs/read_text_file error path=%s error=%s", summarizePath(absPath), summarizeError(err))
		_ = c.writeError(req.ID, -32000, fmt.Sprintf("read file: %v", err))
		return
	}

	c.recordActivity("sent fs/read_text_file path=%s bytes=%d", summarizePath(absPath), len(data))
	_ = c.writeResponse(req.ID, ReadTextFileResponse{Content: string(data)})
}

func (c *Client) handleFSWrite(ctx context.Context, req *JSONRPCRequest) {
	var params WriteTextFileRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("fs/write_text_file parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv fs/write_text_file path=%s content_len=%d", summarizePath(params.Path), len(params.Content))

	absPath, err := c.resolvePath(params.Path)
	if err != nil {
		c.recordActivity("fs/write_text_file error=%s", summarizeError(err))
		_ = c.writeError(req.ID, -32000, err.Error())
		return
	}
	if err := c.authorizeFileTool(ctx, "write_file", absPath, req.Params); err != nil {
		c.recordActivity("fs/write_text_file denied path=%s error=%s", summarizePath(absPath), summarizeError(err))
		_ = c.writeError(req.ID, -32000, err.Error())
		return
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.recordActivity("fs/write_text_file mkdir_error path=%s error=%s", summarizePath(dir), summarizeError(err))
		_ = c.writeError(req.ID, -32000, fmt.Sprintf("mkdir: %v", err))
		return
	}

	if err := os.WriteFile(absPath, []byte(params.Content), 0o644); err != nil {
		c.recordActivity("fs/write_text_file error path=%s error=%s", summarizePath(absPath), summarizeError(err))
		_ = c.writeError(req.ID, -32000, fmt.Sprintf("write file: %v", err))
		return
	}

	c.recordActivity("sent fs/write_text_file path=%s", summarizePath(absPath))
	_ = c.writeResponse(req.ID, WriteTextFileResponse{})
}

// ---------- Permission ----------

func (c *Client) resolvePath(path string) (string, error) {
	c.mu.Lock()
	workingDir := c.workingDir
	c.mu.Unlock()

	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return absPath, nil
}

func (c *Client) authorizeFileTool(ctx context.Context, toolName, absPath string, rawParams json.RawMessage) error {
	if c.policy == nil {
		return nil
	}
	permissionInput, err := json.Marshal(map[string]string{"file_path": absPath})
	if err != nil {
		return fmt.Errorf("encode %s permission input: %w", toolName, err)
	}
	decision, err := c.policy.Check(toolName, permissionInput)
	if err != nil {
		return fmt.Errorf("%s permission check: %w", toolName, err)
	}
	if !c.policy.AllowedPathForTool(toolName, absPath) {
		decision = Deny
	}
	switch decision {
	case Allow:
		return nil
	case Deny:
		return fmt.Errorf("%s denied for %s", toolName, absPath)
	case Ask:
		if c.onApproval == nil {
			return fmt.Errorf("%s requires approval for %s", toolName, absPath)
		}
		if c.onApproval(ctx, toolName, string(rawParams)) != Allow {
			return fmt.Errorf("%s denied for %s", toolName, absPath)
		}
		return nil
	default:
		return fmt.Errorf("%s permission returned unknown decision", toolName)
	}
}

func (c *Client) handlePermission(ctx context.Context, req *JSONRPCRequest) {
	var params RequestPermissionRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("session/request_permission parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv session/request_permission %s", summarizePermissionActivity(params))

	resp, err := c.permissionRequestResponse(ctx, params, req.Params)
	if err != nil {
		c.recordActivity("session/request_permission error=%s", summarizeError(err))
		_ = c.writeError(req.ID, -32000, err.Error())
		return
	}
	c.recordActivity("sent session/request_permission %s", summarizePermissionResponseActivity(resp))
	_ = c.writeResponse(req.ID, resp)
}

func (c *Client) handleFactoryPermission(ctx context.Context, req *JSONRPCRequest) {
	var params FactoryRequestPermissionRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("droid.request_permission parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	acpParams := c.factoryPermissionAsACP(params)
	c.recordActivity("recv droid.request_permission %s", summarizePermissionActivity(acpParams))

	resp, err := c.permissionRequestResponse(ctx, acpParams, req.Params)
	if err != nil {
		c.recordActivity("droid.request_permission error=%s", summarizeError(err))
		_ = c.writeError(req.ID, -32000, err.Error())
		return
	}
	selected := droidPermissionCancel
	if resp.Outcome.SelectedOption != nil {
		selected = c.resolveFactoryPermissionSelection(params.Options, resp.Outcome.SelectedOption.OptionID)
	}
	wireResp := FactoryRequestPermissionResponse{SelectedOption: selected}
	c.recordActivity("sent droid.request_permission selected=%s", summarizeText(wireResp.SelectedOption))
	_ = c.writeResponse(req.ID, wireResp)
}

func (c *Client) handleFactoryAskUser(req *JSONRPCRequest) {
	var params FactoryAskUserRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("droid.ask_user parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv droid.ask_user questions=%d", len(params.Questions))
	resp := FactoryAskUserResponse{
		Cancelled: true,
		Answers:   []FactoryAskUserAnswer{},
	}
	c.recordActivity("sent droid.ask_user cancelled=true")
	_ = c.writeResponse(req.ID, resp)
}

func (c *Client) permissionRequestResponse(ctx context.Context, params RequestPermissionRequest, rawParams json.RawMessage) (RequestPermissionResponse, error) {
	if c.onPermission != nil {
		return c.onPermission(ctx, params)
	}

	toolName, input := c.permissionApprovalContext(params, rawParams)
	if decision, handled, err := c.permissionPolicyDecision(toolName, input); err != nil {
		return RequestPermissionResponse{}, err
	} else if handled {
		return permissionDecisionResponse(params.Options, decision, true), nil
	}
	c.emitPermissionEscalation(params, input)

	if c.onApproval != nil {
		decision := c.onApproval(ctx, toolName, string(input))
		preferPersistent := false
		if policyDecision, handled, err := c.permissionPolicyDecision(toolName, input); err == nil && handled && policyDecision == decision {
			preferPersistent = true
		}
		return permissionDecisionResponse(params.Options, decision, preferPersistent), nil
	}

	return RequestPermissionResponse{
		Outcome: RequestPermissionOutcome{
			Outcome: "rejected",
		},
	}, nil
}

func (c *Client) permissionApprovalContext(params RequestPermissionRequest, rawParams json.RawMessage) (string, json.RawMessage) {
	toolName := "delegate_" + sanitizePermissionKey(c.def.Def.Name) + "_permission"
	input := rawParams
	if params.ToolCall == nil {
		return toolName, input
	}
	kind := string(params.ToolCall.Kind)
	if kind == "" {
		kind = "other"
	}
	toolName = "delegate_" + sanitizePermissionKey(c.def.Def.Name) + "_" + sanitizePermissionKey(kind)
	if len(params.ToolCall.RawInput) > 0 {
		input = params.ToolCall.RawInput
	}
	return toolName, input
}

func (c *Client) emitPermissionEscalation(params RequestPermissionRequest, input json.RawMessage) {
	c.mu.Lock()
	handler := c.onEscalation
	c.mu.Unlock()
	if handler == nil {
		return
	}
	event := PermissionEscalationEvent{
		Type:      "permission_escalation",
		SessionID: params.SessionID,
		Action:    "escalate",
		Message:   "Approval required",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if params.ToolCall != nil {
		event.ToolCallID = params.ToolCall.ToolCallID
		event.ToolTitle = acpToolName(params.ToolCall.Title, params.ToolCall.Kind)
		event.ToolName = strings.TrimSpace(params.ToolCall.Title)
		event.ToolKind = params.ToolCall.Kind
		if len(params.ToolCall.RawInput) > 0 {
			event.ToolInput = append(json.RawMessage(nil), params.ToolCall.RawInput...)
		}
	}
	if len(event.ToolInput) == 0 && len(input) > 0 {
		event.ToolInput = append(json.RawMessage(nil), input...)
	}
	if event.ToolTitle == "" {
		event.ToolTitle = "permission request"
	}
	event.Message = "Approval required for " + event.ToolTitle
	handler(event)
}

func (c *Client) permissionPolicyDecision(toolName string, input json.RawMessage) (Decision, bool, error) {
	if c.policy == nil {
		return Ask, false, nil
	}
	decision, err := c.policy.Check(toolName, input)
	if err != nil {
		return Ask, false, fmt.Errorf("%s permission check: %w", toolName, err)
	}
	if decision == Ask {
		return decision, false, nil
	}
	return decision, true, nil
}

func permissionDecisionResponse(options []PermissionOption, decision Decision, preferPersistent bool) RequestPermissionResponse {
	optionID, ok := selectPermissionOptionID(options, decision, preferPersistent)
	if !ok {
		return RequestPermissionResponse{
			Outcome: RequestPermissionOutcome{
				Outcome: "rejected",
			},
		}
	}
	return RequestPermissionResponse{
		Outcome: RequestPermissionOutcome{
			Outcome: "selected",
			SelectedOption: &SelectedPermissionOutcome{
				OptionID: optionID,
			},
		},
	}
}

func (c *Client) factoryPermissionAsACP(params FactoryRequestPermissionRequest) RequestPermissionRequest {
	acpParams := RequestPermissionRequest{
		Options: make([]PermissionOption, 0, len(params.Options)),
	}
	c.mu.Lock()
	acpParams.SessionID = c.sessionID
	c.mu.Unlock()
	if len(params.ToolUses) > 0 {
		first := params.ToolUses[0]
		acpParams.ToolCall = &ToolCallUpdate{
			ToolCallID: first.ToolUse.ID,
			Title:      first.ToolUse.Name,
			Kind:       factoryConfirmationTypeToToolKind(first.ConfirmationType),
			RawInput:   copyRawMessage(first.ToolUse.Input),
		}
	}
	for _, option := range params.Options {
		acpParams.Options = append(acpParams.Options, PermissionOption{
			OptionID: option.Value,
			Name:     option.Label,
			Kind:     factoryPermissionOptionKind(option.Value),
		})
	}
	return acpParams
}

func factoryConfirmationTypeToToolKind(value string) ToolKind {
	switch strings.TrimSpace(value) {
	case droidConfirmationEdit, droidConfirmationApplyPatch:
		return ToolKindEdit
	case droidConfirmationExec:
		return ToolKindExecute
	case droidConfirmationCreate:
		return ToolKindEdit
	case droidConfirmationAskUser:
		return ToolKindOther
	default:
		return ToolKindOther
	}
}

func factoryPermissionOptionKind(value string) PermissionOptionKind {
	switch strings.TrimSpace(value) {
	case droidPermissionProceedAlways:
		return PermissionOptionAllowAlways
	case droidPermissionCancel:
		return PermissionOptionRejectOnce
	default:
		return PermissionOptionAllowOnce
	}
}

func (c *Client) resolveFactoryPermissionSelection(options []FactoryToolConfirmationOption, optionID string) string {
	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(option.Value), strings.TrimSpace(optionID)) {
			return option.Value
		}
	}
	return droidPermissionCancel
}

func (c *Client) markFactoryPromptProgressLocked() {
	if c.activePromptID == "" {
		return
	}
	c.droidSeenNonIdle = true
}

func (c *Client) mergeFactoryPromptToolUseLocked(toolUse FactoryToolUseBlock) *promptToolState {
	if c.promptToolMeta == nil {
		c.promptToolMeta = make(map[string]*promptToolState)
	}
	toolID := strings.TrimSpace(toolUse.ID)
	state := c.promptToolMeta[toolID]
	if state == nil {
		state = &promptToolState{toolID: toolID}
		c.promptToolMeta[toolID] = state
	}
	state.toolName = strings.TrimSpace(toolUse.Name)
	state.title = strings.TrimSpace(toolUse.Name)
	state.kind = ToolKindOther
	state.status = ToolCallStatusInProgress
	state.rawInput = copyRawMessage(toolUse.Input)
	return state
}

func (c *Client) mergeFactoryPromptToolResultLocked(notif FactorySessionNotificationPayload) *promptToolState {
	if c.promptToolMeta == nil {
		c.promptToolMeta = make(map[string]*promptToolState)
	}
	toolID := strings.TrimSpace(notif.ToolUseID)
	state := c.promptToolMeta[toolID]
	if state == nil {
		state = &promptToolState{toolID: toolID}
		c.promptToolMeta[toolID] = state
	}
	state.rawOutput = copyRawMessage(notif.Content)
	if notif.IsError {
		state.status = ToolCallStatusFailed
	} else {
		state.status = ToolCallStatusCompleted
	}
	return state
}

func selectPermissionOptionID(options []PermissionOption, decision Decision, preferPersistent bool) (string, bool) {
	kinds := []PermissionOptionKind{PermissionOptionRejectOnce, PermissionOptionRejectAlways}
	if preferPersistent {
		kinds = []PermissionOptionKind{PermissionOptionRejectAlways, PermissionOptionRejectOnce}
	}
	if decision == Allow {
		kinds = []PermissionOptionKind{PermissionOptionAllowOnce, PermissionOptionAllowAlways}
		if preferPersistent {
			kinds = []PermissionOptionKind{PermissionOptionAllowAlways, PermissionOptionAllowOnce}
		}
	}
	for _, kind := range kinds {
		for _, option := range options {
			if permissionOptionKindMatches(option.Kind, kind) {
				return option.OptionID, true
			}
		}
	}
	for _, optionID := range preferredPermissionOptionIDs(decision, preferPersistent) {
		for _, option := range options {
			if strings.EqualFold(strings.TrimSpace(option.OptionID), optionID) {
				return option.OptionID, true
			}
		}
	}
	if decision == Allow {
		return "", false
	}
	for _, option := range options {
		if option.Kind == "" || permissionOptionKindMatches(option.Kind, PermissionOptionRejectOnce) || permissionOptionKindMatches(option.Kind, PermissionOptionRejectAlways) {
			return option.OptionID, true
		}
	}
	return "", false
}

func permissionOptionKindMatches(actual, expected PermissionOptionKind) bool {
	if actual == expected {
		return true
	}
	switch expected {
	case PermissionOptionAllowOnce:
		return actual == "allow_once"
	case PermissionOptionAllowAlways:
		return actual == "allow_always"
	case PermissionOptionRejectOnce:
		return actual == "reject_once" || actual == "deny_once"
	case PermissionOptionRejectAlways:
		return actual == "reject_always" || actual == "deny_always"
	default:
		return false
	}
}

func preferredPermissionOptionIDs(decision Decision, preferPersistent bool) []string {
	if decision == Allow {
		if preferPersistent {
			return []string{"approve_for_session", "allow_always", "allow_once", "allow"}
		}
		return []string{"allow_once", "allow_always", "allow", "approve_for_session"}
	}
	if preferPersistent {
		return []string{"reject_always", "deny_always", "reject_once", "deny_once", "reject", "deny", "cancel"}
	}
	return []string{"reject_once", "deny_once", "reject_always", "deny_always", "reject", "deny", "cancel"}
}

func sanitizePermissionKey(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "other"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "other"
	}
	return out
}

// ---------- Terminal (stub — not supported yet) ----------

func (c *Client) handleTerminalCreate(req *JSONRPCRequest) {
	var params CreateTerminalRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("terminal/create parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv terminal/create command=%s cwd=%s rejected=unsupported", summarizeText(params.Command), summarizePath(params.CWD))
	_ = c.writeError(req.ID, -32001, "terminal operations not supported by ggcode ACP host")
}

func (c *Client) handleTerminalOutput(req *JSONRPCRequest) {
	var params TerminalOutputRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("terminal/output parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv terminal/output terminal_id=%s rejected=unsupported", summarizeText(params.TerminalID))
	_ = c.writeError(req.ID, -32001, "terminal operations not supported by ggcode ACP host")
}

func (c *Client) handleTerminalWaitForExit(req *JSONRPCRequest) {
	var params WaitForTerminalExitRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("terminal/wait_for_exit parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv terminal/wait_for_exit terminal_id=%s rejected=unsupported", summarizeText(params.TerminalID))
	_ = c.writeError(req.ID, -32001, "terminal operations not supported by ggcode ACP host")
}

func (c *Client) handleTerminalKill(req *JSONRPCRequest) {
	var params KillTerminalRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("terminal/kill parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv terminal/kill terminal_id=%s rejected=unsupported", summarizeText(params.TerminalID))
	_ = c.writeError(req.ID, -32001, "terminal operations not supported by ggcode ACP host")
}

func (c *Client) handleTerminalRelease(req *JSONRPCRequest) {
	var params ReleaseTerminalRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.recordActivity("terminal/release parse_error=%s", err)
		_ = c.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	c.recordActivity("recv terminal/release terminal_id=%s rejected=unsupported", summarizeText(params.TerminalID))
	_ = c.writeError(req.ID, -32001, "terminal operations not supported by ggcode ACP host")
}
