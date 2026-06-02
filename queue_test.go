package acp

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestFileQueueStoreRoundTrip(t *testing.T) {
	store := NewFileQueueStore(t.TempDir())
	request, err := store.Enqueue("record-1", "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	if request.Status != QueueRequestQueued {
		t.Fatalf("expected queued status, got %q", request.Status)
	}

	loaded, err := store.Load("record-1", request.RequestID)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded == nil || loaded.Prompt != "hello" {
		t.Fatalf("unexpected request: %#v", loaded)
	}
	if loaded.Kind != QueueRequestPrompt {
		t.Fatalf("expected prompt kind, got %#v", loaded)
	}

	loaded.CancelRequested = true
	if err := store.Save(loaded); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	loaded, err = store.Load("record-1", request.RequestID)
	if err != nil {
		t.Fatalf("Load after save error: %v", err)
	}
	if loaded == nil || !loaded.CancelRequested {
		t.Fatalf("expected cancel requested flag, got %#v", loaded)
	}
}

func TestFileQueueStoreControlRequestRoundTrip(t *testing.T) {
	store := NewFileQueueStore(t.TempDir())
	request, err := store.EnqueueSetMode("record-1", "plan")
	if err != nil {
		t.Fatalf("EnqueueSetMode error: %v", err)
	}
	loaded, err := store.Load("record-1", request.RequestID)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded == nil || loaded.Kind != QueueRequestSetMode || loaded.ModeID != "plan" {
		t.Fatalf("unexpected control request: %#v", loaded)
	}
}

func TestFileQueueStoreProbeOwnerCleansStaleLease(t *testing.T) {
	store := NewFileQueueStore(t.TempDir())
	err := store.SaveLease(&QueueOwnerLease{
		RecordID:    "record-1",
		PID:         424242,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
		HeartbeatAt: time.Now().UTC().Add(-time.Hour),
		QueueDepth:  3,
	})
	if err != nil {
		t.Fatalf("SaveLease error: %v", err)
	}
	health, err := store.ProbeOwner("record-1")
	if err != nil {
		t.Fatalf("ProbeOwner error: %v", err)
	}
	if health == nil || health.HasLease {
		t.Fatalf("expected stale lease to be cleared, got %#v", health)
	}
	lease, err := store.LoadLease("record-1")
	if err != nil {
		t.Fatalf("LoadLease error: %v", err)
	}
	if lease != nil {
		t.Fatalf("expected cleared lease, got %#v", lease)
	}
}

func TestFileQueueStoreRawMessagesRoundTrip(t *testing.T) {
	store := NewFileQueueStore(t.TempDir())
	request, err := store.Enqueue("record-1", "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	if err := store.AppendRawMessage("record-1", request.RequestID, json.RawMessage(`{"jsonrpc":"2.0","method":"session/prompt"}`)); err != nil {
		t.Fatalf("AppendRawMessage error: %v", err)
	}
	if err := store.AppendRawMessage("record-1", request.RequestID, json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)); err != nil {
		t.Fatalf("AppendRawMessage error: %v", err)
	}
	lines, err := store.ReadRawMessages("record-1", request.RequestID)
	if err != nil {
		t.Fatalf("ReadRawMessages error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 raw lines, got %#v", lines)
	}
	if string(lines[0]) != `{"jsonrpc":"2.0","method":"session/prompt"}` {
		t.Fatalf("unexpected first raw line: %q", string(lines[0]))
	}
	if string(lines[1]) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("unexpected second raw line: %q", string(lines[1]))
	}
}

func TestFileQueueStoreTryAcquireLeaseIsExclusive(t *testing.T) {
	store := NewFileQueueStore(t.TempDir())
	acquired, err := store.TryAcquireLease(&QueueOwnerLease{
		RecordID:    "record-1",
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("TryAcquireLease error: %v", err)
	}
	if !acquired {
		t.Fatalf("expected first lease acquisition to succeed")
	}
	lease, err := store.LoadLease("record-1")
	if err != nil {
		t.Fatalf("LoadLease error: %v", err)
	}
	if lease == nil || lease.OwnerGeneration == 0 {
		t.Fatalf("expected owner generation to be populated, got %#v", lease)
	}
	acquired, err = store.TryAcquireLease(&QueueOwnerLease{
		RecordID:    "record-1",
		PID:         os.Getpid() + 1,
		StartedAt:   time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("TryAcquireLease second error: %v", err)
	}
	if acquired {
		t.Fatalf("expected second lease acquisition to fail while healthy lease exists")
	}
}

func TestFileQueueStoreRequeueOrphanedRunningRequests(t *testing.T) {
	store := NewFileQueueStore(t.TempDir())
	request, err := store.Enqueue("record-1", "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	started := time.Now().UTC()
	request.Status = QueueRequestRunning
	request.StartedAt = &started
	request.CancelRequested = true
	request.ResultText = "partial"
	request.StopReason = "cancelled"
	request.Error = "boom"
	if err := store.Save(request); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	reset, err := store.RequeueOrphanedRunningRequests("record-1")
	if err != nil {
		t.Fatalf("RequeueOrphanedRunningRequests error: %v", err)
	}
	if len(reset) != 1 {
		t.Fatalf("expected one reset request, got %#v", reset)
	}
	loaded, err := store.Load("record-1", request.RequestID)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded.Status != QueueRequestQueued || loaded.StartedAt != nil || loaded.CancelRequested || loaded.ResultText != "" || loaded.StopReason != "" || loaded.Error != "" {
		t.Fatalf("expected running request to be requeued cleanly, got %#v", loaded)
	}
}
