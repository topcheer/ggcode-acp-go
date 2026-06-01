package acp

// ErrAgentNotFound is returned when the requested agent is not installed.
type ErrAgentNotFound struct {
	name string
}

func (e ErrAgentNotFound) Error() string {
	return "agent " + e.name + " not found — not installed or not in $PATH"
}
