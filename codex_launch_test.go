package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexLaunchEnvBuildsGatewayBootstrapForResponsesProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZAI_API_KEY", "secret-token")
	writeCodexConfig(t, `
model = "glm-5.1"
model_provider = "zai"

[model_providers.zai]
name = "ZAI"
base_url = "https://open.bigmodel.cn/api/coding/paas/v4"
env_key = "ZAI_API_KEY"
wire_api = "responses"
`)

	env, err := codexLaunchEnv()
	if err != nil {
		t.Fatalf("codexLaunchEnv returned error: %v", err)
	}
	if len(env) != 3 {
		t.Fatalf("expected 3 env vars, got %d: %v", len(env), env)
	}
	values := envSliceToMap(env)
	if got := values["MODEL_PROVIDER"]; got != "zai" {
		t.Fatalf("MODEL_PROVIDER = %q, want zai", got)
	}

	var bridged codexConfigFile
	if err := json.Unmarshal([]byte(values["CODEX_CONFIG"]), &bridged); err != nil {
		t.Fatalf("parse CODEX_CONFIG: %v", err)
	}
	if bridged.ModelProvider != "zai" || bridged.Model != "glm-5.1" {
		t.Fatalf("unexpected bridged config: %+v", bridged)
	}
	if got := bridged.ModelProviders["zai"].WireAPI; got != "responses" {
		t.Fatalf("bridged wire_api = %q, want responses", got)
	}

	var auth map[string]any
	if err := json.Unmarshal([]byte(values["DEFAULT_AUTH_REQUEST"]), &auth); err != nil {
		t.Fatalf("parse DEFAULT_AUTH_REQUEST: %v", err)
	}
	if got := auth["methodId"]; got != "gateway" {
		t.Fatalf("methodId = %v, want gateway", got)
	}
	meta := auth["_meta"].(map[string]any)
	gateway := meta["gateway"].(map[string]any)
	if got := gateway["baseUrl"]; got != "https://open.bigmodel.cn/api/coding/paas/v4" {
		t.Fatalf("baseUrl = %v", got)
	}
	headers := gateway["headers"].(map[string]any)
	if got := headers["Authorization"]; got != "Bearer secret-token" {
		t.Fatalf("Authorization header = %v", got)
	}
}

func TestCodexLaunchEnvRejectsChatWireAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeCodexConfig(t, `
model = "glm-5.1"
model_provider = "zai"

[model_providers.zai]
name = "ZAI"
base_url = "https://open.bigmodel.cn/api/coding/paas/v4"
env_key = "ZAI_API_KEY"
wire_api = "chat"
`)

	_, err := codexLaunchEnv()
	if err == nil {
		t.Fatal("expected error for chat wire_api")
	}
	if !strings.Contains(err.Error(), `wire_api="chat"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodexLaunchEnvSkipsWhenConfigMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, err := codexLaunchEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func writeCodexConfig(t *testing.T, body string) {
	t.Helper()
	home := os.Getenv("HOME")
	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	path := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}
