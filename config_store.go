package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RuntimeConfig stores CLI/runtime defaults.
type RuntimeConfig struct {
	DefaultAgent       string `json:"defaultAgent,omitempty"`
	DefaultSessionName string `json:"defaultSessionName,omitempty"`
}

// ConfigStore persists runtime configuration.
type ConfigStore interface {
	Load() (*RuntimeConfig, error)
	Save(*RuntimeConfig) error
}

// FileConfigStore stores runtime config as JSON.
type FileConfigStore struct {
	stateDir string
}

func NewFileConfigStore(stateDir string) *FileConfigStore {
	return &FileConfigStore{stateDir: filepath.Clean(stateDir)}
}

func (s *FileConfigStore) filePath() string {
	return filepath.Join(s.stateDir, "config.json")
}

func (s *FileConfigStore) Load() (*RuntimeConfig, error) {
	payload, err := os.ReadFile(s.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &RuntimeConfig{}, nil
		}
		return nil, fmt.Errorf("reading runtime config: %w", err)
	}
	var cfg RuntimeConfig
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("decoding runtime config: %w", err)
	}
	return &cfg, nil
}

func (s *FileConfigStore) Save(cfg *RuntimeConfig) error {
	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		return fmt.Errorf("creating runtime config directory: %w", err)
	}
	if cfg == nil {
		cfg = &RuntimeConfig{}
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding runtime config: %w", err)
	}
	return os.WriteFile(s.filePath(), append(payload, '\n'), 0o644)
}
