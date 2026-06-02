package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Transport manages JSON-RPC 2.0 communication over stdio.
// All writes to the writer are protected by a mutex to ensure atomic messages.
// Supports bi-directional communication: both responding to Client requests
// and sending Agent→Client requests (e.g. session/request_permission, fs/read_text_file).
type Transport struct {
	Scanner      *bufio.Scanner
	Writer       io.Writer
	mu           sync.Mutex
	rawMu        sync.RWMutex
	rawHook      func(json.RawMessage)
	wireProtocol WireProtocol

	// Bi-directional request support
	nextID    atomic.Int64
	pending   map[string]chan *JSONRPCResponse
	pendingMu sync.Mutex
}

// NewTransport creates a new Transport reading from r and writing to w.
func NewTransport(r io.Reader, w io.Writer) *Transport {
	return NewTransportWithProtocol(r, w, WireProtocolACP)
}

// NewTransportWithProtocol creates a new transport configured for the given wire protocol.
func NewTransportWithProtocol(r io.Reader, w io.Writer, protocol WireProtocol) *Transport {
	s := bufio.NewScanner(r)
	// Copilot ACP can emit large single-line NDJSON payloads for tool results.
	s.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	return &Transport{
		Scanner:      s,
		Writer:       w,
		pending:      make(map[string]chan *JSONRPCResponse),
		wireProtocol: normalizeWireProtocol(protocol),
	}
}

func (t *Transport) SetRawMessageObserver(hook func(json.RawMessage)) {
	t.rawMu.Lock()
	defer t.rawMu.Unlock()
	t.rawHook = hook
}

func (t *Transport) emitRawMessage(line []byte) {
	if len(line) == 0 {
		return
	}
	t.rawMu.RLock()
	hook := t.rawHook
	t.rawMu.RUnlock()
	if hook == nil {
		return
	}
	raw := append(json.RawMessage(nil), line...)
	hook(raw)
}

// WriteRaw writes a raw byte slice to the transport followed by a newline.
// This is primarily useful for testing — production code should use
// SendMessage or SendNotification.
func (t *Transport) WriteRaw(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.Writer.Write(data); err != nil {
		return err
	}
	_, err := t.Writer.Write([]byte("\n"))
	return err
}

// ReadRaw reads the next raw line from the transport. Returns the bytes
// without parsing. Returns io.EOF when the input stream is closed.
// This is primarily useful for testing.
func (t *Transport) ReadRaw() ([]byte, error) {
	if !t.Scanner.Scan() {
		if err := t.Scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading raw: %w", err)
		}
		return nil, io.EOF
	}
	return t.Scanner.Bytes(), nil
}

// CloseWriter closes the underlying writer if it implements io.Closer.
func (t *Transport) CloseWriter() error {
	if c, ok := t.Writer.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// ReadMessage reads and parses the next JSON-RPC request from stdin.
// Returns io.EOF when the input stream is closed.
func (t *Transport) ReadMessage() (*JSONRPCRequest, error) {
	if !t.Scanner.Scan() {
		if err := t.Scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading message: %w", err)
		}
		return nil, io.EOF
	}
	line := t.Scanner.Bytes()
	if len(line) == 0 {
		return nil, nil // skip empty lines
	}
	var req JSONRPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("parsing JSON-RPC message: %w", err)
	}
	return &req, nil
}

// ReadAnyMessage reads the next JSON-RPC message, which can be either
// a request (from Client) or a response (to a pending Agent→Client request).
// Returns (request, nil, nil) for Client requests,
// (nil, response, nil) for responses to our pending requests,
// (nil, nil, nil) for empty lines (skip),
// (nil, nil, io.EOF) when stream ends.
func (t *Transport) ReadAnyMessage() (*JSONRPCRequest, *JSONRPCResponse, error) {
	if !t.Scanner.Scan() {
		if err := t.Scanner.Err(); err != nil {
			return nil, nil, fmt.Errorf("reading message: %w", err)
		}
		return nil, nil, io.EOF
	}
	line := t.Scanner.Bytes()
	if len(line) == 0 {
		return nil, nil, nil // skip empty lines
	}

	// Try to determine if this is a response or a request
	// A response has "result" or "error" fields; a request has "method"
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, nil, fmt.Errorf("parsing JSON-RPC message: %w", err)
	}

	// If it has a "method" field, it's a request
	if _, hasMethod := raw["method"]; hasMethod {
		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return nil, nil, fmt.Errorf("parsing JSON-RPC request: %w", err)
		}
		t.emitRawMessage(line)
		return &req, nil, nil
	}

	// Otherwise it's a response
	var resp JSONRPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, nil, fmt.Errorf("parsing JSON-RPC response: %w", err)
	}
	// Preserve raw result for SendRequest callers
	if rawResult, ok := raw["result"]; ok {
		resp.RawResult = rawResult
	}
	t.emitRawMessage(line)
	return nil, &resp, nil
}

// SendRequest sends a JSON-RPC request to the Client and waits for the response.
// This is used for Agent→Client requests like session/request_permission and fs/read_text_file.
// Returns the raw response result or an error.
func (t *Transport) SendRequest(method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	requestID := interface{}(id)
	pendingKey := strconv.FormatInt(id, 10)
	if t.isFactoryProtocol() {
		requestID = pendingKey
	}

	// Register pending response channel before sending
	ch := make(chan *JSONRPCResponse, 1)
	t.pendingMu.Lock()
	t.pending[pendingKey] = ch
	t.pendingMu.Unlock()
	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, pendingKey)
		t.pendingMu.Unlock()
	}()

	// Build and send request
	req := struct {
		JSONRPC                string          `json:"jsonrpc"`
		FactoryAPIVersion      string          `json:"factoryApiVersion,omitempty"`
		FactoryProtocolVersion string          `json:"factoryProtocolVersion,omitempty"`
		Type                   string          `json:"type,omitempty"`
		ID                     interface{}     `json:"id"`
		Method                 string          `json:"method"`
		Params                 json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      requestID,
		Method:  method,
	}
	if t.isFactoryProtocol() {
		req.FactoryAPIVersion = factoryAPIVersion
		req.FactoryProtocolVersion = factoryProtocolVersion
		req.Type = "request"
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshaling request params: %w", err)
		}
		req.Params = raw
	}

	if err := t.writeJSON(req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	// Wait for response
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("client error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.RawResult, nil
	case <-timer.C:
		return nil, fmt.Errorf("timeout waiting for client response to %s", method)
	}
}

// DeliverResponse delivers a response from the Client to a pending SendRequest caller.
// Called by the Handler's message loop when it receives a JSON-RPC response.
func (t *Transport) DeliverResponse(resp *JSONRPCResponse) {
	if resp.ID == nil {
		return
	}
	id, ok := transportResponseIDKey(resp.ID)
	if !ok {
		return
	}

	t.pendingMu.Lock()
	ch, ok := t.pending[id]
	t.pendingMu.Unlock()

	if ok {
		ch <- resp
	}
}

// WriteResponse writes a JSON-RPC response with the given result.
func (t *Transport) WriteResponse(id RequestID, result interface{}) error {
	resp := struct {
		JSONRPC                string      `json:"jsonrpc"`
		FactoryAPIVersion      string      `json:"factoryApiVersion,omitempty"`
		FactoryProtocolVersion string      `json:"factoryProtocolVersion,omitempty"`
		Type                   string      `json:"type,omitempty"`
		ID                     RequestID   `json:"id"`
		Result                 interface{} `json:"result,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	if t.isFactoryProtocol() {
		resp.FactoryAPIVersion = factoryAPIVersion
		resp.FactoryProtocolVersion = factoryProtocolVersion
		resp.Type = "response"
	}
	return t.writeJSON(resp)
}

// WriteError writes a JSON-RPC error response.
func (t *Transport) WriteError(id RequestID, code int, msg string) error {
	resp := struct {
		JSONRPC                string        `json:"jsonrpc"`
		FactoryAPIVersion      string        `json:"factoryApiVersion,omitempty"`
		FactoryProtocolVersion string        `json:"factoryProtocolVersion,omitempty"`
		Type                   string        `json:"type,omitempty"`
		ID                     RequestID     `json:"id"`
		Error                  *JSONRPCError `json:"error,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: msg,
		},
	}
	if t.isFactoryProtocol() {
		resp.FactoryAPIVersion = factoryAPIVersion
		resp.FactoryProtocolVersion = factoryProtocolVersion
		resp.Type = "response"
	}
	return t.writeJSON(resp)
}

// WriteErrorNilID writes a JSON-RPC error response with a nil ID.
func (t *Transport) WriteErrorNilID(code int, msg string) error {
	resp := struct {
		JSONRPC                string        `json:"jsonrpc"`
		FactoryAPIVersion      string        `json:"factoryApiVersion,omitempty"`
		FactoryProtocolVersion string        `json:"factoryProtocolVersion,omitempty"`
		Type                   string        `json:"type,omitempty"`
		Error                  *JSONRPCError `json:"error,omitempty"`
	}{
		JSONRPC: "2.0",
		Error: &JSONRPCError{
			Code:    code,
			Message: msg,
		},
	}
	if t.isFactoryProtocol() {
		resp.FactoryAPIVersion = factoryAPIVersion
		resp.FactoryProtocolVersion = factoryProtocolVersion
		resp.Type = "response"
	}
	return t.writeJSON(resp)
}

// WriteNotification writes a JSON-RPC notification (no ID).
func (t *Transport) WriteNotification(method string, params interface{}) error {
	notif := struct {
		JSONRPC                string          `json:"jsonrpc"`
		FactoryAPIVersion      string          `json:"factoryApiVersion,omitempty"`
		FactoryProtocolVersion string          `json:"factoryProtocolVersion,omitempty"`
		Type                   string          `json:"type,omitempty"`
		Method                 string          `json:"method"`
		Params                 json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		Method:  method,
	}
	if t.isFactoryProtocol() {
		notif.FactoryAPIVersion = factoryAPIVersion
		notif.FactoryProtocolVersion = factoryProtocolVersion
		notif.Type = "notification"
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshaling notification params: %w", err)
		}
		notif.Params = raw
	}
	return t.writeJSON(notif)
}

func (t *Transport) isFactoryProtocol() bool {
	return normalizeWireProtocol(t.wireProtocol) == WireProtocolFactoryJSONRPC
}

func transportResponseIDKey(value interface{}) (string, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case float64:
		return strconv.FormatInt(int64(v), 10), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	default:
		return "", false
	}
}

// writeJSON marshals v and writes it as a single line followed by \n.
// Protected by mutex to ensure atomic writes.
func (t *Transport) writeJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling JSON-RPC message: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.Writer.Write(data); err != nil {
		return fmt.Errorf("writing JSON-RPC message: %w", err)
	}
	if _, err := t.Writer.Write([]byte("\n")); err != nil {
		return fmt.Errorf("writing newline: %w", err)
	}
	t.emitRawMessage(data)
	return nil
}
