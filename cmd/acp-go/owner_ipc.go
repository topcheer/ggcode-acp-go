package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/topcheer/ggcode-acp-go"
)

type ownerIPCSubmitPromptRequest struct {
	Type       string `json:"type"`
	Prompt     string `json:"prompt"`
	RequestID  string `json:"requestId,omitempty"`
	ModeID     string `json:"modeId,omitempty"`
	ModelID    string `json:"modelId,omitempty"`
	ConfigID   string `json:"configId,omitempty"`
	Value      string `json:"value,omitempty"`
	Wait       bool   `json:"wait,omitempty"`
	JSONOutput bool   `json:"jsonOutput,omitempty"`
}

type ownerIPCResult struct {
	Status     string `json:"status,omitempty"`
	Text       string `json:"text,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

type ownerIPCEnvelope struct {
	Type            string            `json:"type"`
	RequestID       string            `json:"requestId,omitempty"`
	OwnerGeneration int64             `json:"ownerGeneration,omitempty"`
	Cancelled       bool              `json:"cancelled,omitempty"`
	Closed          bool              `json:"closed,omitempty"`
	ModeID          string            `json:"modeId,omitempty"`
	ModelID         string            `json:"modelId,omitempty"`
	ConfigID        string            `json:"configId,omitempty"`
	Value           string            `json:"value,omitempty"`
	Event           *acp.RuntimeEvent `json:"event,omitempty"`
	Message         json.RawMessage   `json:"message,omitempty"`
	Result          *ownerIPCResult   `json:"result,omitempty"`
	Error           string            `json:"error,omitempty"`
}

type ownerIPCPromptStreamResult struct {
	RequestID string
	Text      string
	Status    string
}

type ownerPromptSubscriber struct {
	conn     net.Conn
	encoder  *json.Encoder
	wantRaw  bool
	done     chan struct{}
	doneOnce sync.Once
	writeMu  sync.Mutex
}

func (s *ownerPromptSubscriber) finish() {
	s.doneOnce.Do(func() {
		_ = s.conn.Close()
		close(s.done)
	})
}

func (s *ownerPromptSubscriber) writeEnvelope(envelope ownerIPCEnvelope) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.encoder.Encode(envelope); err != nil {
		s.finish()
		return err
	}
	return nil
}

type ownerIPCServer struct {
	stateDir   string
	recordID   string
	generation int64
	socketPath string
	queue      *acp.FileQueueStore
	manager    *acp.RuntimeManager
	wakeCh     chan struct{}

	listener net.Listener

	mu               sync.Mutex
	subscribers      map[string]*ownerPromptSubscriber
	cancelers        map[string]func()
	activeController acp.RuntimeActiveSessionController
	closeRequested   bool
}

func ownerIPCRequestsDir(stateDir string) string {
	return filepath.Join(filepath.Clean(stateDir), "queue", "sockets")
}

func ownerIPCSocketPath(stateDir, recordID string) string {
	return filepath.Join(ownerIPCRequestsDir(stateDir), url.PathEscape(recordID)+".sock")
}

func queueLeaseGeneration(queue *acp.FileQueueStore, recordID string) int64 {
	lease, err := queue.LoadLease(recordID)
	if err != nil || lease == nil {
		return 0
	}
	return lease.OwnerGeneration
}

func newOwnerIPCServer(stateDir, recordID string, generation int64, queue *acp.FileQueueStore, manager *acp.RuntimeManager, wakeCh chan struct{}) *ownerIPCServer {
	return &ownerIPCServer{
		stateDir:    stateDir,
		recordID:    recordID,
		generation:  generation,
		socketPath:  ownerIPCSocketPath(stateDir, recordID),
		queue:       queue,
		manager:     manager,
		wakeCh:      wakeCh,
		subscribers: make(map[string]*ownerPromptSubscriber),
		cancelers:   make(map[string]func()),
	}
}

func (s *ownerIPCServer) Start() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("creating owner ipc directory: %w", err)
	}
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale owner ipc socket: %w", err)
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("starting owner ipc listener: %w", err)
	}
	s.listener = listener
	go s.acceptLoop()
	return nil
}

func (s *ownerIPCServer) Close() error {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.mu.Lock()
	for requestID, subscriber := range s.subscribers {
		delete(s.subscribers, requestID)
		subscriber.finish()
	}
	s.mu.Unlock()
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *ownerIPCServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *ownerIPCServer) handleConn(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	if !scanner.Scan() {
		_ = conn.Close()
		return
	}
	var request ownerIPCSubmitPromptRequest
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           fmt.Sprintf("decoding owner ipc request: %v", err),
		})
		_ = conn.Close()
		return
	}
	switch request.Type {
	case "submit_prompt":
		s.handleSubmitPrompt(conn, request)
	case "cancel_prompt":
		s.handleCancelPrompt(conn, request)
	case "set_mode":
		s.handleSetMode(conn, request)
	case "set_model":
		s.handleSetModel(conn, request)
	case "set_config_option":
		s.handleSetConfig(conn, request)
	case "close_session":
		s.handleCloseSession(conn, request)
	default:
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           fmt.Sprintf("unsupported owner ipc request %q", request.Type),
		})
		_ = conn.Close()
	}
}

func (s *ownerIPCServer) handleSubmitPrompt(conn net.Conn, request ownerIPCSubmitPromptRequest) {
	if request.Prompt == "" {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           "prompt text is required",
		})
		_ = conn.Close()
		return
	}
	queueRequest, err := s.queue.Enqueue(s.recordID, request.Prompt)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           err.Error(),
		})
		_ = conn.Close()
		return
	}
	encoder := json.NewEncoder(conn)
	subscriber := &ownerPromptSubscriber{
		conn:    conn,
		encoder: encoder,
		wantRaw: request.JSONOutput,
		done:    make(chan struct{}),
	}
	if request.Wait {
		s.registerSubscriber(queueRequest.RequestID, subscriber)
	}
	if err := subscriber.writeEnvelope(ownerIPCEnvelope{
		Type:            "accepted",
		RequestID:       queueRequest.RequestID,
		OwnerGeneration: s.generation,
	}); err != nil {
		s.unregisterSubscriber(queueRequest.RequestID)
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
	if !request.Wait {
		subscriber.finish()
		return
	}
	<-subscriber.done
}

func (s *ownerIPCServer) handleCancelPrompt(conn net.Conn, request ownerIPCSubmitPromptRequest) {
	defer conn.Close()
	if strings.TrimSpace(request.RequestID) == "" {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           "requestId is required",
		})
		return
	}
	cancelled, err := s.CancelPrompt(request.RequestID)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			RequestID:       request.RequestID,
			OwnerGeneration: s.generation,
			Error:           err.Error(),
		})
		return
	}
	_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
		Type:            "cancel_result",
		RequestID:       request.RequestID,
		OwnerGeneration: s.generation,
		Cancelled:       cancelled,
	})
}

func (s *ownerIPCServer) handleSetMode(conn net.Conn, request ownerIPCSubmitPromptRequest) {
	defer conn.Close()
	modeID := strings.TrimSpace(request.ModeID)
	if modeID == "" {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           "modeId is required",
		})
		return
	}
	if err := s.SetSessionMode(modeID); err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           err.Error(),
		})
		return
	}
	_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
		Type:            "set_mode_result",
		OwnerGeneration: s.generation,
		ModeID:          modeID,
	})
}

func (s *ownerIPCServer) handleSetModel(conn net.Conn, request ownerIPCSubmitPromptRequest) {
	defer conn.Close()
	modelID := strings.TrimSpace(request.ModelID)
	if modelID == "" {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           "modelId is required",
		})
		return
	}
	if err := s.SetSessionModel(modelID); err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           err.Error(),
		})
		return
	}
	_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
		Type:            "set_model_result",
		OwnerGeneration: s.generation,
		ModelID:         modelID,
	})
}

func (s *ownerIPCServer) handleSetConfig(conn net.Conn, request ownerIPCSubmitPromptRequest) {
	defer conn.Close()
	configID := strings.TrimSpace(request.ConfigID)
	value := strings.TrimSpace(request.Value)
	if configID == "" {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           "configId is required",
		})
		return
	}
	if value == "" {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           "value is required",
		})
		return
	}
	if err := s.SetSessionConfigOption(configID, value); err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           err.Error(),
		})
		return
	}
	_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
		Type:            "set_config_option_result",
		OwnerGeneration: s.generation,
		ConfigID:        configID,
		Value:           value,
	})
}

func (s *ownerIPCServer) handleCloseSession(conn net.Conn, _ ownerIPCSubmitPromptRequest) {
	defer conn.Close()
	closed, err := s.CloseSession()
	if err != nil {
		_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
			Type:            "error",
			OwnerGeneration: s.generation,
			Error:           err.Error(),
		})
		return
	}
	_ = json.NewEncoder(conn).Encode(ownerIPCEnvelope{
		Type:            "close_session_result",
		OwnerGeneration: s.generation,
		Closed:          closed,
	})
}

func (s *ownerIPCServer) registerSubscriber(requestID string, subscriber *ownerPromptSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers[requestID] = subscriber
}

func (s *ownerIPCServer) unregisterSubscriber(requestID string) {
	s.mu.Lock()
	subscriber := s.subscribers[requestID]
	delete(s.subscribers, requestID)
	s.mu.Unlock()
	if subscriber != nil {
		subscriber.finish()
	}
}

func (s *ownerIPCServer) RegisterPromptCanceler(requestID string, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelers[requestID] = cancel
}

func (s *ownerIPCServer) UnregisterPromptCanceler(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancelers, requestID)
}

func (s *ownerIPCServer) RegisterActiveController(controller acp.RuntimeActiveSessionController) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeController = controller
}

func (s *ownerIPCServer) UnregisterActiveController() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeController = nil
}

func (s *ownerIPCServer) subscriber(requestID string) *ownerPromptSubscriber {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribers[requestID]
}

func (s *ownerIPCServer) currentActiveController() acp.RuntimeActiveSessionController {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeController
}

func (s *ownerIPCServer) markCloseRequested() {
	s.mu.Lock()
	s.closeRequested = true
	s.mu.Unlock()
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *ownerIPCServer) ShouldClose() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeRequested
}

func (s *ownerIPCServer) CancelPrompt(requestID string) (bool, error) {
	record, err := s.queue.Load(s.recordID, requestID)
	if err != nil {
		return false, err
	}
	if record == nil || queueRequestTerminal(record.Status) {
		return false, nil
	}
	if _, err := s.queue.RequestCancel(s.recordID, requestID); err != nil {
		return false, err
	}
	s.mu.Lock()
	cancel := s.cancelers[requestID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true, nil
}

func (s *ownerIPCServer) SetSessionMode(modeID string) error {
	if active := s.currentActiveController(); active != nil {
		if err := active.SetSessionMode(context.Background(), acp.SessionModeId(modeID)); err != nil {
			return err
		}
		return persistQueuedControlState(s.stateDir, s.recordID, modeID, "", "", "", false)
	}
	return s.manager.SetSessionMode(context.Background(), s.recordID, acp.SessionModeId(modeID))
}

func (s *ownerIPCServer) SetSessionModel(modelID string) error {
	if active := s.currentActiveController(); active != nil {
		if err := active.SetSessionModel(context.Background(), modelID); err != nil {
			return err
		}
		return persistQueuedControlState(s.stateDir, s.recordID, "", modelID, "", "", false)
	}
	return s.manager.SetSessionModel(context.Background(), s.recordID, modelID)
}

func (s *ownerIPCServer) SetSessionConfigOption(configID, value string) error {
	if active := s.currentActiveController(); active != nil {
		if _, err := active.SetSessionConfigOption(context.Background(), acp.SessionConfigId(configID), acp.SessionConfigValueId(value)); err != nil {
			return err
		}
		return persistQueuedControlState(s.stateDir, s.recordID, "", "", configID, value, false)
	}
	return s.manager.SetSessionConfigOption(context.Background(), s.recordID, acp.SessionConfigId(configID), acp.SessionConfigValueId(value))
}

func (s *ownerIPCServer) CloseSession() (bool, error) {
	if active := s.currentActiveController(); active != nil {
		record, err := acp.NewFileSessionStore(s.stateDir).Load(s.recordID)
		if err != nil {
			return false, err
		}
		if record != nil && strings.TrimSpace(record.ActiveRequestID) != "" {
			_, _ = s.CancelPrompt(record.ActiveRequestID)
		}
		if err := active.CloseSession(context.Background()); err != nil {
			return false, err
		}
		if err := persistQueuedControlState(s.stateDir, s.recordID, "", "", "", "", true); err != nil {
			return false, err
		}
		s.markCloseRequested()
		return true, nil
	}
	if _, err := s.manager.CloseSession(context.Background(), s.recordID); err != nil {
		return false, err
	}
	s.markCloseRequested()
	return true, nil
}

func (s *ownerIPCServer) EmitEvent(requestID string, event acp.RuntimeEvent) {
	subscriber := s.subscriber(requestID)
	if subscriber == nil || subscriber.wantRaw {
		return
	}
	if err := subscriber.writeEnvelope(ownerIPCEnvelope{
		Type:            "event",
		RequestID:       requestID,
		OwnerGeneration: s.generation,
		Event:           &event,
	}); err != nil {
		s.unregisterSubscriber(requestID)
	}
}

func (s *ownerIPCServer) EmitRawMessage(requestID string, message json.RawMessage) {
	subscriber := s.subscriber(requestID)
	if subscriber == nil || !subscriber.wantRaw {
		return
	}
	if err := subscriber.writeEnvelope(ownerIPCEnvelope{
		Type:            "event",
		RequestID:       requestID,
		OwnerGeneration: s.generation,
		Message:         append(json.RawMessage(nil), message...),
	}); err != nil {
		s.unregisterSubscriber(requestID)
	}
}

func (s *ownerIPCServer) EmitResult(requestID string, request *acp.QueueRequestRecord) {
	subscriber := s.subscriber(requestID)
	if subscriber == nil {
		return
	}
	_ = subscriber.writeEnvelope(ownerIPCEnvelope{
		Type:            "result",
		RequestID:       requestID,
		OwnerGeneration: s.generation,
		Result: &ownerIPCResult{
			Status:     string(request.Status),
			Text:       request.ResultText,
			StopReason: request.StopReason,
		},
	})
	s.unregisterSubscriber(requestID)
}

func (s *ownerIPCServer) EmitError(requestID string, err error) {
	subscriber := s.subscriber(requestID)
	if subscriber == nil {
		return
	}
	_ = subscriber.writeEnvelope(ownerIPCEnvelope{
		Type:            "error",
		RequestID:       requestID,
		OwnerGeneration: s.generation,
		Error:           err.Error(),
	})
	s.unregisterSubscriber(requestID)
}

func streamQueuedPromptViaOwnerSocket(stateDir, recordID, prompt string, jsonOutput bool, stdout io.Writer, onEvent func(acp.RuntimeEvent)) (*ownerIPCPromptStreamResult, bool, error) {
	queue := acp.NewFileQueueStore(stateDir)
	lease, err := queue.LoadLease(recordID)
	if err != nil {
		return nil, false, err
	}
	if !acp.QueueOwnerLeaseAlive(lease) || lease == nil || lease.SocketPath == "" {
		return nil, false, nil
	}
	conn, err := net.DialTimeout("unix", lease.SocketPath, time.Second)
	if err != nil {
		return nil, false, nil
	}
	accepted := &ownerIPCPromptStreamResult{}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(ownerIPCSubmitPromptRequest{
		Type:       "submit_prompt",
		Prompt:     prompt,
		Wait:       true,
		JSONOutput: jsonOutput,
	}); err != nil {
		_ = conn.Close()
		return nil, false, nil
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var envelope ownerIPCEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			_ = conn.Close()
			if accepted.RequestID != "" {
				return accepted, false, err
			}
			return nil, false, nil
		}
		if lease.OwnerGeneration != 0 && envelope.OwnerGeneration != 0 && envelope.OwnerGeneration != lease.OwnerGeneration {
			_ = conn.Close()
			if accepted.RequestID != "" {
				return accepted, false, fmt.Errorf("owner generation mismatch")
			}
			return nil, false, nil
		}
		switch envelope.Type {
		case "accepted":
			accepted.RequestID = envelope.RequestID
		case "event":
			if jsonOutput && len(envelope.Message) > 0 {
				emitRawJSONMessage(stdout, envelope.Message)
			}
			if !jsonOutput && envelope.Event != nil && onEvent != nil {
				onEvent(*envelope.Event)
			}
		case "result":
			_ = conn.Close()
			if envelope.Result != nil {
				accepted.Text = envelope.Result.Text
				accepted.Status = envelope.Result.Status
			}
			return accepted, true, nil
		case "error":
			_ = conn.Close()
			if accepted.RequestID != "" {
				return accepted, true, fmt.Errorf("%s", envelope.Error)
			}
			return nil, true, fmt.Errorf("%s", envelope.Error)
		}
	}
	_ = conn.Close()
	if accepted.RequestID != "" {
		if err := scanner.Err(); err != nil {
			return accepted, false, err
		}
		return accepted, false, io.ErrUnexpectedEOF
	}
	return nil, false, scanner.Err()
}

func requestOwnerSocketCancel(stateDir, recordID, requestID string) (bool, bool, error) {
	queue := acp.NewFileQueueStore(stateDir)
	lease, err := queue.LoadLease(recordID)
	if err != nil {
		return false, false, err
	}
	if lease == nil || !acp.QueueOwnerLeaseAlive(lease) || lease.SocketPath == "" {
		return false, false, nil
	}
	conn, err := net.DialTimeout("unix", lease.SocketPath, time.Second)
	if err != nil {
		return false, false, nil
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(ownerIPCSubmitPromptRequest{
		Type:      "cancel_prompt",
		RequestID: requestID,
	}); err != nil {
		return false, false, nil
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, false, err
		}
		return false, false, nil
	}
	var envelope ownerIPCEnvelope
	if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
		return false, false, err
	}
	if lease.OwnerGeneration != 0 && envelope.OwnerGeneration != 0 && lease.OwnerGeneration != envelope.OwnerGeneration {
		return false, false, nil
	}
	switch envelope.Type {
	case "cancel_result":
		return true, envelope.Cancelled, nil
	case "error":
		return true, false, fmt.Errorf("%s", envelope.Error)
	default:
		return false, false, nil
	}
}

func requestOwnerSocketSetMode(stateDir, recordID, modeID string) (bool, error) {
	envelope, handled, err := requestOwnerSocketControl(stateDir, recordID, ownerIPCSubmitPromptRequest{
		Type:   "set_mode",
		ModeID: modeID,
	})
	if err != nil || !handled {
		return handled, err
	}
	if envelope.Type != "set_mode_result" {
		return false, nil
	}
	return true, nil
}

func requestOwnerSocketSetModel(stateDir, recordID, modelID string) (bool, error) {
	envelope, handled, err := requestOwnerSocketControl(stateDir, recordID, ownerIPCSubmitPromptRequest{
		Type:    "set_model",
		ModelID: modelID,
	})
	if err != nil || !handled {
		return handled, err
	}
	if envelope.Type != "set_model_result" {
		return false, nil
	}
	return true, nil
}

func requestOwnerSocketSetConfig(stateDir, recordID, configID, value string) (bool, error) {
	envelope, handled, err := requestOwnerSocketControl(stateDir, recordID, ownerIPCSubmitPromptRequest{
		Type:     "set_config_option",
		ConfigID: configID,
		Value:    value,
	})
	if err != nil || !handled {
		return handled, err
	}
	if envelope.Type != "set_config_option_result" {
		return false, nil
	}
	return true, nil
}

func requestOwnerSocketClose(stateDir, recordID string) (bool, bool, error) {
	envelope, handled, err := requestOwnerSocketControl(stateDir, recordID, ownerIPCSubmitPromptRequest{
		Type: "close_session",
	})
	if err != nil || !handled {
		return handled, false, err
	}
	if envelope.Type != "close_session_result" {
		return false, false, nil
	}
	return true, envelope.Closed, nil
}

func requestOwnerSocketControl(stateDir, recordID string, request ownerIPCSubmitPromptRequest) (ownerIPCEnvelope, bool, error) {
	queue := acp.NewFileQueueStore(stateDir)
	lease, err := queue.LoadLease(recordID)
	if err != nil {
		return ownerIPCEnvelope{}, false, err
	}
	if lease == nil || !acp.QueueOwnerLeaseAlive(lease) || lease.SocketPath == "" {
		return ownerIPCEnvelope{}, false, nil
	}
	conn, err := net.DialTimeout("unix", lease.SocketPath, time.Second)
	if err != nil {
		return ownerIPCEnvelope{}, false, nil
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return ownerIPCEnvelope{}, false, nil
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return ownerIPCEnvelope{}, false, err
		}
		return ownerIPCEnvelope{}, false, nil
	}
	var envelope ownerIPCEnvelope
	if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
		return ownerIPCEnvelope{}, false, err
	}
	if lease.OwnerGeneration != 0 && envelope.OwnerGeneration != 0 && lease.OwnerGeneration != envelope.OwnerGeneration {
		return ownerIPCEnvelope{}, false, nil
	}
	switch envelope.Type {
	case "error":
		return envelope, true, fmt.Errorf("%s", envelope.Error)
	default:
		return envelope, true, nil
	}
}
