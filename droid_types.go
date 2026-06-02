package acp

import (
	"bytes"
	"encoding/json"
	"strings"
)

type WireProtocol string

const (
	WireProtocolACP                        WireProtocol = "acp"
	WireProtocolFactoryJSONRPC             WireProtocol = "factory-stream-jsonrpc"
	factoryAPIVersion                                   = "1.0.0"
	factoryProtocolVersion                              = "1.51.0"
	droidMethodInitializeSession                        = "droid.initialize_session"
	droidMethodLoadSession                              = "droid.load_session"
	droidMethodAddUserMessage                           = "droid.add_user_message"
	droidMethodCloseSession                             = "droid.close_session"
	droidMethodInterruptSession                         = "droid.interrupt_session"
	droidMethodSessionNotification                      = "droid.session_notification"
	droidMethodRequestPermission                        = "droid.request_permission"
	droidMethodAskUser                                  = "droid.ask_user"
	droidNotificationToolResult                         = "tool_result"
	droidNotificationCreateMessage                      = "create_message"
	droidNotificationError                              = "error"
	droidNotificationWorkingStateChanged                = "droid_working_state_changed"
	droidNotificationSessionTitleUpdated                = "session_title_updated"
	droidNotificationAssistantTextDelta                 = "assistant_text_delta"
	droidNotificationAssistantTextComplete              = "assistant_text_complete"
	droidNotificationThinkingTextDelta                  = "thinking_text_delta"
	droidNotificationThinkingTextComplete               = "thinking_text_complete"
	droidNotificationToolCall                           = "tool_call"
	droidNotificationSettingsUpdated                    = "settings_updated"
	droidNotificationTokenUsageChanged                  = "session_token_usage_changed"
	droidWorkingStateIdle                               = "idle"
	droidPermissionProceedOnce                          = "proceed_once"
	droidPermissionProceedAlways                        = "proceed_always"
	droidPermissionProceedAutoRun                       = "proceed_auto_run"
	droidPermissionProceedAutoRunLow                    = "proceed_auto_run_low"
	droidPermissionProceedAutoRunMedium                 = "proceed_auto_run_medium"
	droidPermissionProceedAutoRunHigh                   = "proceed_auto_run_high"
	droidPermissionProceedNewSession                    = "proceed_new_session"
	droidPermissionProceedNewSessionLow                 = "proceed_new_session_low"
	droidPermissionProceedNewSessionMedium              = "proceed_new_session_medium"
	droidPermissionProceedNewSessionHigh                = "proceed_new_session_high"
	droidPermissionProceedEdit                          = "proceed_edit"
	droidPermissionCancel                               = "cancel"
	droidConfirmationEdit                               = "edit"
	droidConfirmationExec                               = "exec"
	droidConfirmationCreate                             = "create"
	droidConfirmationAskUser                            = "ask_user"
	droidConfirmationApplyPatch                         = "apply_patch"
)

func normalizeWireProtocol(value WireProtocol) WireProtocol {
	switch value {
	case "", WireProtocolACP:
		return WireProtocolACP
	case WireProtocolFactoryJSONRPC:
		return WireProtocolFactoryJSONRPC
	default:
		return WireProtocolACP
	}
}

type FactoryInitializeSessionRequest struct {
	MachineID string `json:"machineId"`
	CWD       string `json:"cwd"`
	ModelID   string `json:"modelId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type FactoryInitializeSessionResponse struct {
	SessionID string          `json:"sessionId"`
	Session   json.RawMessage `json:"session,omitempty"`
}

type FactoryLoadSessionRequest struct {
	SessionID string `json:"sessionId"`
}

type FactoryLoadSessionResponse struct {
	Session json.RawMessage `json:"session,omitempty"`
	CWD     string          `json:"cwd,omitempty"`
}

type FactoryAddUserMessageRequest struct {
	Text string `json:"text"`
}

type FactoryCloseSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

type FactoryInterruptSessionRequest struct{}

type FactoryToolUseBlock struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type FactoryToolConfirmationInfo struct {
	ToolUse          FactoryToolUseBlock `json:"toolUse"`
	ConfirmationType string              `json:"confirmationType,omitempty"`
	Details          json.RawMessage     `json:"details,omitempty"`
}

type FactoryToolConfirmationOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type FactoryRequestPermissionRequest struct {
	ToolUses []FactoryToolConfirmationInfo   `json:"toolUses"`
	Options  []FactoryToolConfirmationOption `json:"options"`
}

type FactoryRequestPermissionResponse struct {
	SelectedOption string `json:"selectedOption"`
	Comment        string `json:"comment,omitempty"`
}

type FactoryAskUserQuestion struct {
	Index    int      `json:"index"`
	Topic    string   `json:"topic,omitempty"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

type FactoryAskUserRequest struct {
	ToolCallID string                   `json:"toolCallId"`
	Questions  []FactoryAskUserQuestion `json:"questions"`
}

type FactoryAskUserAnswer struct {
	Index    int    `json:"index"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type FactoryAskUserResponse struct {
	Cancelled bool                   `json:"cancelled,omitempty"`
	Answers   []FactoryAskUserAnswer `json:"answers"`
}

type FactorySessionNotificationParams struct {
	Notification FactorySessionNotificationPayload `json:"notification"`
}

type FactorySessionNotificationPayload struct {
	Type           string               `json:"type"`
	MessageID      string               `json:"messageId,omitempty"`
	BlockIndex     int                  `json:"blockIndex,omitempty"`
	TextDelta      string               `json:"textDelta,omitempty"`
	ToolUse        *FactoryToolUseBlock `json:"toolUse,omitempty"`
	ToolUseID      string               `json:"toolUseId,omitempty"`
	Content        json.RawMessage      `json:"content,omitempty"`
	IsError        bool                 `json:"isError,omitempty"`
	Message        *FactoryMessage      `json:"-"`
	MessageText    string               `json:"-"`
	NewState       string               `json:"newState,omitempty"`
	Title          string               `json:"title,omitempty"`
	SessionID      string               `json:"sessionId,omitempty"`
	TokenUsage     json.RawMessage      `json:"tokenUsage,omitempty"`
	Settings       json.RawMessage      `json:"settings,omitempty"`
	RequestID      string               `json:"requestId,omitempty"`
	ToolUseIDs     []string             `json:"toolUseIds,omitempty"`
	SelectedOption string               `json:"selectedOption,omitempty"`
}

type FactoryMessage struct {
	ID         string                      `json:"id"`
	Role       string                      `json:"role"`
	Content    []FactoryMessageContentItem `json:"content"`
	Visibility string                      `json:"visibility,omitempty"`
}

type FactoryMessageContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func extractFactoryMessageText(message *FactoryMessage) string {
	if message == nil || len(message.Content) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, item := range message.Content {
		if item.Type == "text" {
			builder.WriteString(item.Text)
		}
	}
	return builder.String()
}

func copyRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawJSONValue(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text
	}
	return string(trimmed)
}

func (p *FactorySessionNotificationPayload) UnmarshalJSON(data []byte) error {
	type wirePayload struct {
		Type           string               `json:"type"`
		MessageID      string               `json:"messageId,omitempty"`
		BlockIndex     int                  `json:"blockIndex,omitempty"`
		TextDelta      string               `json:"textDelta,omitempty"`
		ToolUse        *FactoryToolUseBlock `json:"toolUse,omitempty"`
		ToolUseID      string               `json:"toolUseId,omitempty"`
		Content        json.RawMessage      `json:"content,omitempty"`
		MessageRaw     json.RawMessage      `json:"message,omitempty"`
		NewState       string               `json:"newState,omitempty"`
		Title          string               `json:"title,omitempty"`
		SessionID      string               `json:"sessionId,omitempty"`
		TokenUsage     json.RawMessage      `json:"tokenUsage,omitempty"`
		Settings       json.RawMessage      `json:"settings,omitempty"`
		RequestID      string               `json:"requestId,omitempty"`
		ToolUseIDs     []string             `json:"toolUseIds,omitempty"`
		SelectedOption string               `json:"selectedOption,omitempty"`
	}

	var wire wirePayload
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*p = FactorySessionNotificationPayload{
		Type:           wire.Type,
		MessageID:      wire.MessageID,
		BlockIndex:     wire.BlockIndex,
		TextDelta:      wire.TextDelta,
		ToolUse:        wire.ToolUse,
		ToolUseID:      wire.ToolUseID,
		Content:        wire.Content,
		NewState:       wire.NewState,
		Title:          wire.Title,
		SessionID:      wire.SessionID,
		TokenUsage:     wire.TokenUsage,
		Settings:       wire.Settings,
		RequestID:      wire.RequestID,
		ToolUseIDs:     wire.ToolUseIDs,
		SelectedOption: wire.SelectedOption,
	}
	if len(bytes.TrimSpace(wire.MessageRaw)) == 0 || bytes.Equal(bytes.TrimSpace(wire.MessageRaw), []byte("null")) {
		return nil
	}
	var message FactoryMessage
	if err := json.Unmarshal(wire.MessageRaw, &message); err == nil && (message.Role != "" || len(message.Content) > 0 || message.Visibility != "") {
		p.Message = &message
		return nil
	}
	var text string
	if err := json.Unmarshal(wire.MessageRaw, &text); err == nil {
		p.MessageText = text
		return nil
	}
	return nil
}
