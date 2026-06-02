package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStaticAgentRegistryResolveAliasAndOverride(t *testing.T) {
	registry := NewStaticAgentRegistry(map[string]AgentDef{
		"custom": {
			Title:         "Custom",
			Description:   "Custom ACP entrypoint",
			Command:       "custom-acp",
			Args:          []string{"serve"},
			CheckBinaries: []string{"custom-acp"},
		},
	})

	def, err := registry.Resolve("factory-droid")
	if err != nil {
		t.Fatalf("Resolve alias error: %v", err)
	}
	if def.Name != "droid" {
		t.Fatalf("expected droid alias, got %q", def.Name)
	}

	custom, err := registry.Resolve("custom")
	if err != nil {
		t.Fatalf("Resolve custom error: %v", err)
	}
	if custom.Command != "custom-acp" {
		t.Fatalf("expected custom command, got %q", custom.Command)
	}
}

func TestFileSessionStoreRoundTripListAndDelete(t *testing.T) {
	store := NewFileSessionStore(t.TempDir())

	first := &SessionRecord{
		RecordID:   "alpha",
		SessionKey: "workspace:/tmp/project",
		Agent:      "copilot",
		CWD:        "/tmp/project",
	}
	if err := store.Save(first); err != nil {
		t.Fatalf("Save first record error: %v", err)
	}

	second := &SessionRecord{
		RecordID:   "beta",
		SessionKey: "workspace:/tmp/project:alt",
		Agent:      "ggcode",
		CWD:        "/tmp/project",
	}
	if err := store.Save(second); err != nil {
		t.Fatalf("Save second record error: %v", err)
	}

	loaded, err := store.Load("alpha")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded == nil || loaded.Agent != "copilot" {
		t.Fatalf("unexpected loaded record: %#v", loaded)
	}

	byKey, err := store.FindByKey("workspace:/tmp/project")
	if err != nil {
		t.Fatalf("FindByKey error: %v", err)
	}
	if byKey == nil || byKey.RecordID != "alpha" {
		t.Fatalf("unexpected FindByKey record: %#v", byKey)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	if err := store.Delete("alpha"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	loaded, err = store.Load("alpha")
	if err != nil {
		t.Fatalf("Load after delete error: %v", err)
	}
	if loaded != nil {
		t.Fatalf("expected deleted record to be nil, got %#v", loaded)
	}
}

func TestFileSessionStoreUsesSessionsDirectory(t *testing.T) {
	root := t.TempDir()
	store := NewFileSessionStore(root)
	record := &SessionRecord{
		RecordID: "encoded/id",
		Agent:    "copilot",
		CWD:      "/tmp/project",
	}
	if err := store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatalf("ReadDir error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 stored file, got %d", len(entries))
	}
	if entries[0].Name() == "encoded/id.json" {
		t.Fatalf("expected escaped file name, got %q", entries[0].Name())
	}
}
