package acp

import "testing"

func TestFileConfigStoreRoundTrip(t *testing.T) {
	store := NewFileConfigStore(t.TempDir())
	cfg := &RuntimeConfig{
		DefaultAgent:       "copilot",
		DefaultSessionName: "workspace",
	}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded.DefaultAgent != "copilot" || loaded.DefaultSessionName != "workspace" {
		t.Fatalf("unexpected config: %#v", loaded)
	}
}
