package acp

type PromptEventType string

const (
	PromptEventText       PromptEventType = "text"
	PromptEventToolCall   PromptEventType = "tool_call"
	PromptEventToolResult PromptEventType = "tool_result"
)

type PromptEvent struct {
	Type      PromptEventType
	Text      string
	ToolID    string
	ToolName  string
	ToolTitle string
	ToolArgs  string
	Result    string
	IsError   bool
}
