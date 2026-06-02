package acp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTransportRawMessageObserverSeesOutboundAndInbound(t *testing.T) {
	var written bytes.Buffer
	transport := NewTransport(strings.NewReader("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"hi\"}}}}\n"), &written)
	var seen []string
	transport.SetRawMessageObserver(func(message json.RawMessage) {
		seen = append(seen, string(message))
	})

	if err := transport.WriteNotification("session/cancel", map[string]string{"sessionId": "sess-1"}); err != nil {
		t.Fatalf("WriteNotification error: %v", err)
	}
	req, resp, err := transport.ReadAnyMessage()
	if err != nil {
		t.Fatalf("ReadAnyMessage error: %v", err)
	}
	if req == nil || resp != nil {
		t.Fatalf("expected inbound request, got req=%#v resp=%#v", req, resp)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 raw messages, got %#v", seen)
	}
	if !strings.Contains(seen[0], "\"method\":\"session/cancel\"") {
		t.Fatalf("expected outbound message in observer, got %q", seen[0])
	}
	if !strings.Contains(seen[1], "\"method\":\"session/update\"") {
		t.Fatalf("expected inbound message in observer, got %q", seen[1])
	}
}

func TestTransportFactoryProtocolWritesFactoryEnvelope(t *testing.T) {
	var written bytes.Buffer
	transport := NewTransportWithProtocol(strings.NewReader(""), &written, WireProtocolFactoryJSONRPC)

	if err := transport.WriteNotification(droidMethodSessionNotification, FactorySessionNotificationParams{
		Notification: FactorySessionNotificationPayload{Type: droidNotificationAssistantTextDelta, TextDelta: "hi"},
	}); err != nil {
		t.Fatalf("WriteNotification error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(written.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal written payload: %v", err)
	}
	if raw["type"] != "notification" {
		t.Fatalf("expected factory notification type, got %#v", raw["type"])
	}
	if raw["factoryApiVersion"] != factoryAPIVersion {
		t.Fatalf("expected factoryApiVersion %q, got %#v", factoryAPIVersion, raw["factoryApiVersion"])
	}
	if raw["factoryProtocolVersion"] != factoryProtocolVersion {
		t.Fatalf("expected factoryProtocolVersion %q, got %#v", factoryProtocolVersion, raw["factoryProtocolVersion"])
	}
}
