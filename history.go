package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionHistoryEntry captures a durable prompt/session event.
type SessionHistoryEntry struct {
	Timestamp time.Time       `json:"timestamp"`
	Kind      string          `json:"kind"`
	Role      string          `json:"role,omitempty"`
	Text      string          `json:"text,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`
	ToolID    string          `json:"toolId,omitempty"`
	ToolTitle string          `json:"toolTitle,omitempty"`
	ToolArgs  string          `json:"toolArgs,omitempty"`
	IsError   bool            `json:"isError,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// HistoryStore persists per-session history entries.
type HistoryStore interface {
	Append(recordID string, entry SessionHistoryEntry) error
	Read(recordID string) ([]SessionHistoryEntry, error)
	Replace(recordID string, entries []SessionHistoryEntry) error
	Delete(recordID string) error
}

// FileHistoryStore stores session histories as ndjson files.
type FileHistoryStore struct {
	stateDir string
}

func NewFileHistoryStore(stateDir string) *FileHistoryStore {
	return &FileHistoryStore{stateDir: filepath.Clean(stateDir)}
}

func (s *FileHistoryStore) historiesDir() string {
	return filepath.Join(s.stateDir, "history")
}

func (s *FileHistoryStore) filePath(recordID string) string {
	return filepath.Join(s.historiesDir(), safeRecordFileName(recordID)+".ndjson")
}

func (s *FileHistoryStore) ensureDir() error {
	return os.MkdirAll(s.historiesDir(), 0o755)
}

func normalizeHistoryEntry(entry SessionHistoryEntry) SessionHistoryEntry {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	return entry
}

func (s *FileHistoryStore) Append(recordID string, entry SessionHistoryEntry) error {
	if err := s.ensureDir(); err != nil {
		return fmt.Errorf("creating history store directory: %w", err)
	}
	entry = normalizeHistoryEntry(entry)
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding history entry: %w", err)
	}
	file, err := os.OpenFile(s.filePath(recordID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening history file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("writing history entry: %w", err)
	}
	return nil
}

func (s *FileHistoryStore) Read(recordID string) ([]SessionHistoryEntry, error) {
	if err := s.ensureDir(); err != nil {
		return nil, fmt.Errorf("creating history store directory: %w", err)
	}
	file, err := os.Open(s.filePath(recordID))
	if err != nil {
		if os.IsNotExist(err) {
			return []SessionHistoryEntry{}, nil
		}
		return nil, fmt.Errorf("opening history file: %w", err)
	}
	defer file.Close()

	var entries []SessionHistoryEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry SessionHistoryEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("decoding history entry: %w", err)
		}
		entries = append(entries, normalizeHistoryEntry(entry))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading history file: %w", err)
	}
	return entries, nil
}

func (s *FileHistoryStore) Replace(recordID string, entries []SessionHistoryEntry) error {
	if err := s.ensureDir(); err != nil {
		return fmt.Errorf("creating history store directory: %w", err)
	}
	file := s.filePath(recordID)
	tmp := file + ".tmp"
	fh, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating temp history file: %w", err)
	}
	enc := json.NewEncoder(fh)
	for _, entry := range entries {
		if err := enc.Encode(normalizeHistoryEntry(entry)); err != nil {
			_ = fh.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("encoding history entry: %w", err)
		}
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing temp history file: %w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("committing history file: %w", err)
	}
	return nil
}

func (s *FileHistoryStore) Delete(recordID string) error {
	if err := os.Remove(s.filePath(recordID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting history file: %w", err)
	}
	return nil
}
