package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeManagerImportExportAndPrune(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})

	record := &SessionRecord{
		RecordID:   "record-1",
		SessionKey: "copilot|/tmp/project|default",
		Agent:      "copilot",
		Name:       "default",
		CWD:        "/tmp/project",
		Mode:       string(RuntimeSessionModePersistent),
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save record error: %v", err)
	}
	if err := manager.history.Append(record.RecordID, SessionHistoryEntry{Kind: "prompt", Role: "user", Text: "hello"}); err != nil {
		t.Fatalf("Append history error: %v", err)
	}

	exportPath := filepath.Join(root, "session-export.json")
	if err := manager.ExportSession(record.RecordID, exportPath); err != nil {
		t.Fatalf("ExportSession error: %v", err)
	}

	importRoot := t.TempDir()
	importManager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: importRoot,
		Store:    NewFileSessionStore(importRoot),
		History:  NewFileHistoryStore(importRoot),
		Registry: NewStaticAgentRegistry(nil),
	})
	imported, err := importManager.ImportSession(exportPath)
	if err != nil {
		t.Fatalf("ImportSession error: %v", err)
	}
	if imported.RecordID != record.RecordID {
		t.Fatalf("expected imported record %q, got %q", record.RecordID, imported.RecordID)
	}

	history, err := importManager.ReadHistory(record.RecordID)
	if err != nil {
		t.Fatalf("ReadHistory error: %v", err)
	}
	if len(history) != 1 || history[0].Text != "hello" {
		t.Fatalf("unexpected imported history: %#v", history)
	}

	now := record.UpdatedAt.Add(-time.Hour)
	record.Closed = true
	record.ClosedAt = &now
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save closed record error: %v", err)
	}
	deleted, err := manager.PruneClosedSessions(0)
	if err != nil {
		t.Fatalf("PruneClosedSessions error: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != record.RecordID {
		t.Fatalf("unexpected pruned record ids: %#v", deleted)
	}
}

func TestRuntimeManagerImportSessionWithOptionsOverridesScope(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	record := &SessionRecord{
		RecordID:   "import-override",
		SessionKey: "copilot|/tmp/project|default",
		Agent:      "copilot",
		Name:       "default",
		CWD:        "/tmp/project",
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	exportPath := filepath.Join(root, "session-export.json")
	if err := manager.ExportSession(record.RecordID, exportPath); err != nil {
		t.Fatalf("ExportSession error: %v", err)
	}

	importRoot := t.TempDir()
	importManager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: importRoot,
		Store:    NewFileSessionStore(importRoot),
		History:  NewFileHistoryStore(importRoot),
		Registry: NewStaticAgentRegistry(nil),
	})
	imported, err := importManager.ImportSessionWithOptions(exportPath, ImportSessionOptions{
		Name: "renamed",
		CWD:  filepath.Join(importRoot, "workspace"),
	})
	if err != nil {
		t.Fatalf("ImportSessionWithOptions error: %v", err)
	}
	if imported.Name != "renamed" || imported.CWD != filepath.Join(importRoot, "workspace") {
		t.Fatalf("unexpected import overrides: %#v", imported)
	}
}

func TestRuntimeManagerImportSessionRekeysOnCollision(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	record := &SessionRecord{
		RecordID:   "import-collision",
		SessionKey: "copilot|/tmp/project|default",
		Agent:      "copilot",
		Name:       "default",
		CWD:        "/tmp/project",
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	exportPath := filepath.Join(root, "session-export.json")
	if err := manager.ExportSession(record.RecordID, exportPath); err != nil {
		t.Fatalf("ExportSession error: %v", err)
	}
	imported, err := manager.ImportSession(exportPath)
	if err != nil {
		t.Fatalf("ImportSession error: %v", err)
	}
	if imported.RecordID == record.RecordID {
		t.Fatalf("expected import collision to allocate a new record id")
	}
}

func TestRuntimeManagerStatus(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	record := &SessionRecord{
		RecordID:         "status-1",
		SessionKey:       "copilot|/tmp/project|default",
		Agent:            "copilot",
		CWD:              "/tmp/project",
		Name:             "default",
		BackendSessionID: "backend-1",
		Mode:             string(RuntimeSessionModePersistent),
		Summary:          "ok",
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	status, err := manager.Status(record.RecordID)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if status == nil || status.RecordID != record.RecordID || status.BackendSessionID != "backend-1" {
		t.Fatalf("unexpected status: %#v", status)
	}
	if status.Status != "idle" {
		t.Fatalf("expected idle status, got %#v", status)
	}
}

func TestRuntimeManagerStatusShowsRunningOwner(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	record := &SessionRecord{
		RecordID: "status-running",
		Agent:    "copilot",
		CWD:      "/tmp/project",
		Name:     "default",
		Mode:     string(RuntimeSessionModePersistent),
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	queue := NewFileQueueStore(root)
	if err := queue.SaveLease(&QueueOwnerLease{
		RecordID:    record.RecordID,
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC().Add(-2 * time.Second),
		HeartbeatAt: time.Now().UTC(),
		QueueDepth:  2,
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	status, err := manager.Status(record.RecordID)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if status.Status != "running" || !status.OwnerHealthy || status.QueueDepth != 2 {
		t.Fatalf("unexpected running status: %#v", status)
	}
}

func TestRuntimeManagerExportSessionRejectsActiveOwner(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	record := &SessionRecord{
		RecordID: "export-locked",
		Agent:    "copilot",
		CWD:      "/tmp/project",
		Name:     "default",
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	queue := NewFileQueueStore(root)
	if err := queue.SaveLease(&QueueOwnerLease{
		RecordID:    record.RecordID,
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	err := manager.ExportSession(record.RecordID, filepath.Join(root, "session.json"))
	if err == nil || !strings.Contains(err.Error(), "running queue owner") {
		t.Fatalf("expected active owner export error, got %v", err)
	}
}

func TestRuntimeManagerPruneClosedSessionsDryRunKeepsHistory(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	closedAt := time.Now().UTC().Add(-time.Hour)
	record := &SessionRecord{
		RecordID: "prune-dry-run",
		Agent:    "copilot",
		CWD:      "/tmp/project",
		Closed:   true,
		ClosedAt: &closedAt,
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	if err := manager.history.Append(record.RecordID, SessionHistoryEntry{Kind: "text", Text: "hello"}); err != nil {
		t.Fatalf("Append history error: %v", err)
	}
	result, err := manager.PruneClosedSessionsWithOptions(PruneClosedSessionsOptions{
		DryRun:         true,
		IncludeHistory: false,
	})
	if err != nil {
		t.Fatalf("PruneClosedSessionsWithOptions error: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != record.RecordID {
		t.Fatalf("unexpected dry-run result: %#v", result)
	}
	loaded, err := manager.store.Load(record.RecordID)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded == nil {
		t.Fatalf("expected dry-run to keep record")
	}
	history, err := manager.history.Read(record.RecordID)
	if err != nil {
		t.Fatalf("ReadHistory error: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected history to remain, got %#v", history)
	}
}

func TestPromptEventToHistoryEntryPreservesToolMetadata(t *testing.T) {
	entry := promptEventToHistoryEntry(PromptEvent{
		Type:      PromptEventToolCall,
		ToolID:    "call-1",
		ToolName:  "bash",
		ToolTitle: "Run Bash",
		ToolArgs:  "{\"command\":\"echo hi\"}",
	})
	if entry.ToolID != "call-1" || entry.ToolName != "bash" || entry.ToolTitle != "Run Bash" || entry.ToolArgs != "{\"command\":\"echo hi\"}" {
		t.Fatalf("unexpected history entry: %#v", entry)
	}
}

func TestDefaultStateDirRespectsEnv(t *testing.T) {
	t.Setenv("GGCODE_ACP_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	if got, want := DefaultStateDir(), os.Getenv("GGCODE_ACP_STATE_DIR"); got != want {
		t.Fatalf("DefaultStateDir() = %q, want %q", got, want)
	}
}

func TestRuntimeManagerFindSessionWalksParents(t *testing.T) {
	root := t.TempDir()
	manager := NewRuntimeManager(RuntimeManagerOptions{
		StateDir: root,
		Store:    NewFileSessionStore(root),
		History:  NewFileHistoryStore(root),
		Registry: NewStaticAgentRegistry(nil),
	})
	record := &SessionRecord{
		RecordID:   "walk-1",
		SessionKey: "copilot|" + filepath.Join(root, "project") + "|default",
		Agent:      "copilot",
		Name:       "default",
		CWD:        filepath.Join(root, "project"),
	}
	if err := manager.store.Save(record); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	found, err := manager.FindSession("copilot", filepath.Join(root, "project", "subdir"), "default", true)
	if err != nil {
		t.Fatalf("FindSession error: %v", err)
	}
	if found == nil || found.RecordID != "walk-1" {
		t.Fatalf("unexpected record: %#v", found)
	}
}

func TestRuntimeManagerConstructs(t *testing.T) {
	manager := NewRuntimeManager(RuntimeManagerOptions{})
	if manager == nil {
		t.Fatal("expected manager")
	}
	if manager.registry == nil || manager.store == nil || manager.history == nil {
		t.Fatal("expected default runtime dependencies")
	}
}
