package acp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const SessionRecordSchema = "acp.session.v1"

// SessionRecord is the durable session metadata used by the higher-level runtime/CLI.
type SessionRecord struct {
	Schema             string                 `json:"schema"`
	RecordID           string                 `json:"recordId"`
	SessionKey         string                 `json:"sessionKey,omitempty"`
	Backend            string                 `json:"backend,omitempty"`
	RuntimeSessionName string                 `json:"runtimeSessionName,omitempty"`
	Agent              string                 `json:"agent"`
	Name               string                 `json:"name,omitempty"`
	Title              string                 `json:"title,omitempty"`
	CWD                string                 `json:"cwd"`
	BackendSessionID   string                 `json:"backendSessionId,omitempty"`
	AgentSessionID     string                 `json:"agentSessionId,omitempty"`
	Mode               string                 `json:"mode,omitempty"`
	AvailableModes     []string               `json:"availableModes,omitempty"`
	AvailableModels    []string               `json:"availableModels,omitempty"`
	Summary            string                 `json:"summary,omitempty"`
	LastStopReason     string                 `json:"lastStopReason,omitempty"`
	LastError          string                 `json:"lastError,omitempty"`
	LastPrompt         string                 `json:"lastPrompt,omitempty"`
	LastPromptAt       *time.Time             `json:"lastPromptAt,omitempty"`
	HistoryPath        string                 `json:"historyPath,omitempty"`
	ActiveRequestID    string                 `json:"activeRequestId,omitempty"`
	OwnerPID           int                    `json:"ownerPid,omitempty"`
	ConfigValues       map[string]string      `json:"configValues,omitempty"`
	ConfigOptions      []SessionConfigOption  `json:"configOptions,omitempty"`
	AvailableCommands  []AvailableCommand     `json:"availableCommands,omitempty"`
	SessionOptions     *SessionOptions        `json:"sessionOptions,omitempty"`
	CreatedAt          time.Time              `json:"createdAt"`
	UpdatedAt          time.Time              `json:"updatedAt"`
	Closed             bool                   `json:"closed,omitempty"`
	ClosedAt           *time.Time             `json:"closedAt,omitempty"`
	Metadata           map[string]interface{} `json:"metadata,omitempty"`
}

// SessionStore persists product-level session records.
type SessionStore interface {
	Load(recordID string) (*SessionRecord, error)
	Save(record *SessionRecord) error
	Delete(recordID string) error
	List() ([]*SessionRecord, error)
	FindByKey(sessionKey string) (*SessionRecord, error)
}

// FileSessionStore stores session records as JSON files under a state directory.
type FileSessionStore struct {
	stateDir string
}

func NewFileSessionStore(stateDir string) *FileSessionStore {
	return &FileSessionStore{stateDir: filepath.Clean(stateDir)}
}

func (s *FileSessionStore) sessionsDir() string {
	return filepath.Join(s.stateDir, "sessions")
}

func safeRecordFileName(recordID string) string {
	return url.PathEscape(recordID) + ".json"
}

func (s *FileSessionStore) filePath(recordID string) string {
	return filepath.Join(s.sessionsDir(), safeRecordFileName(recordID))
}

func (s *FileSessionStore) ensureDir() error {
	return os.MkdirAll(s.sessionsDir(), 0o755)
}

func normalizeSessionRecord(record *SessionRecord) *SessionRecord {
	if record == nil {
		return nil
	}
	clone := *record
	if clone.Schema == "" {
		clone.Schema = SessionRecordSchema
	}
	if clone.RecordID == "" {
		clone.RecordID = generateSessionID()
	}
	now := time.Now().UTC()
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	if clone.Metadata != nil {
		metadata := make(map[string]interface{}, len(clone.Metadata))
		for key, value := range clone.Metadata {
			metadata[key] = value
		}
		clone.Metadata = metadata
	}
	if clone.ConfigValues != nil {
		configValues := make(map[string]string, len(clone.ConfigValues))
		for key, value := range clone.ConfigValues {
			configValues[key] = value
		}
		clone.ConfigValues = configValues
	}
	if len(clone.AvailableModes) > 0 {
		clone.AvailableModes = append([]string(nil), clone.AvailableModes...)
	}
	if len(clone.AvailableModels) > 0 {
		clone.AvailableModels = append([]string(nil), clone.AvailableModels...)
	}
	clone.ConfigOptions = cloneSessionConfigOptions(clone.ConfigOptions)
	clone.AvailableCommands = cloneAvailableCommands(clone.AvailableCommands)
	clone.SessionOptions = cloneSessionOptions(clone.SessionOptions)
	return &clone
}

func (s *FileSessionStore) Load(recordID string) (*SessionRecord, error) {
	if err := s.ensureDir(); err != nil {
		return nil, fmt.Errorf("creating session store directory: %w", err)
	}
	payload, err := os.ReadFile(s.filePath(recordID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading session record %q: %w", recordID, err)
	}
	var record SessionRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, fmt.Errorf("decoding session record %q: %w", recordID, err)
	}
	return normalizeSessionRecord(&record), nil
}

func (s *FileSessionStore) Save(record *SessionRecord) error {
	normalized := normalizeSessionRecord(record)
	if normalized == nil {
		return fmt.Errorf("session record is nil")
	}
	if strings.TrimSpace(normalized.Agent) == "" {
		return fmt.Errorf("session record %q missing agent", normalized.RecordID)
	}
	if strings.TrimSpace(normalized.CWD) == "" {
		return fmt.Errorf("session record %q missing cwd", normalized.RecordID)
	}
	if err := s.ensureDir(); err != nil {
		return fmt.Errorf("creating session store directory: %w", err)
	}
	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding session record %q: %w", normalized.RecordID, err)
	}
	file := s.filePath(normalized.RecordID)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing temp session record %q: %w", normalized.RecordID, err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("committing session record %q: %w", normalized.RecordID, err)
	}
	return nil
}

func (s *FileSessionStore) Delete(recordID string) error {
	if err := os.Remove(s.filePath(recordID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting session record %q: %w", recordID, err)
	}
	return nil
}

func (s *FileSessionStore) List() ([]*SessionRecord, error) {
	if err := s.ensureDir(); err != nil {
		return nil, fmt.Errorf("creating session store directory: %w", err)
	}
	entries, err := os.ReadDir(s.sessionsDir())
	if err != nil {
		return nil, fmt.Errorf("listing session records: %w", err)
	}
	records := make([]*SessionRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(s.sessionsDir(), entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading session record %q: %w", entry.Name(), err)
		}
		var record SessionRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, fmt.Errorf("decoding session record %q: %w", entry.Name(), err)
		}
		records = append(records, normalizeSessionRecord(&record))
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
	return records, nil
}

func (s *FileSessionStore) FindByKey(sessionKey string) (*SessionRecord, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	normalized := strings.TrimSpace(sessionKey)
	for _, record := range records {
		if record.SessionKey == normalized {
			return record, nil
		}
	}
	return nil, nil
}
