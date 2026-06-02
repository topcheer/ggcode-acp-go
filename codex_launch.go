package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type codexConfigFile struct {
	Model          string                         `toml:"model"`
	ModelProvider  string                         `toml:"model_provider"`
	ModelProviders map[string]codexProviderConfig `toml:"model_providers"`
}

type codexProviderConfig struct {
	Name        string            `toml:"name"`
	BaseURL     string            `toml:"base_url"`
	EnvKey      string            `toml:"env_key"`
	WireAPI     string            `toml:"wire_api"`
	HTTPHeaders map[string]string `toml:"http_headers"`
}

func launchEnvForAgent(agentName string) ([]string, error) {
	if agentName != "codex" {
		return nil, nil
	}
	return codexLaunchEnv()
}

func codexLaunchEnv() ([]string, error) {
	cfg, err := readCodexConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	providerID := strings.TrimSpace(cfg.ModelProvider)
	if providerID == "" {
		return nil, nil
	}
	provider, ok := cfg.ModelProviders[providerID]
	if !ok {
		return nil, nil
	}

	wireAPI := strings.ToLower(strings.TrimSpace(provider.WireAPI))
	if wireAPI == "chat" {
		return nil, fmt.Errorf(
			"codex ACP adapter does not support model_provider %q with wire_api=%q; codex app-server now requires wire_api=%q for ACP sessions. Direct `codex exec` may still work, but `codex-acp` will not",
			providerID, provider.WireAPI, "responses",
		)
	}

	payload := codexConfigFile{
		Model:         cfg.Model,
		ModelProvider: providerID,
		ModelProviders: map[string]codexProviderConfig{
			providerID: provider,
		},
	}
	configJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal codex config bridge: %w", err)
	}

	env := []string{
		"MODEL_PROVIDER=" + providerID,
		"CODEX_CONFIG=" + string(configJSON),
	}

	defaultAuthJSON, ok, err := buildCodexDefaultAuthRequest(providerID, provider)
	if err != nil {
		return nil, err
	}
	if ok {
		env = append(env, "DEFAULT_AUTH_REQUEST="+defaultAuthJSON)
	}
	return env, nil
}

func buildCodexDefaultAuthRequest(providerID string, provider codexProviderConfig) (string, bool, error) {
	if strings.ToLower(strings.TrimSpace(provider.WireAPI)) != "responses" {
		return "", false, nil
	}
	baseURL := strings.TrimSpace(provider.BaseURL)
	if baseURL == "" {
		return "", false, nil
	}

	headers := make(map[string]string, len(provider.HTTPHeaders)+1)
	for key, value := range provider.HTTPHeaders {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		headers[key] = value
	}
	if envKey := strings.TrimSpace(provider.EnvKey); envKey != "" {
		apiKey := strings.TrimSpace(os.Getenv(envKey))
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
	}
	if len(headers) == 0 {
		return "", false, nil
	}

	providerName := strings.TrimSpace(provider.Name)
	if providerName == "" {
		providerName = providerID
	}
	authRequest := map[string]any{
		"methodId": "gateway",
		"_meta": map[string]any{
			"gateway": map[string]any{
				"baseUrl":      baseURL,
				"headers":      headers,
				"providerName": providerName,
			},
		},
	}
	data, err := json.Marshal(authRequest)
	if err != nil {
		return "", false, fmt.Errorf("marshal codex default auth request: %w", err)
	}
	return string(data), true, nil
}

func readCodexConfig() (codexConfigFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexConfigFile{}, fmt.Errorf("resolve home directory for codex config: %w", err)
	}
	path := filepath.Join(home, ".codex", "config.toml")
	var cfg codexConfigFile
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return codexConfigFile{}, os.ErrNotExist
		}
		return codexConfigFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	return cfg, nil
}
