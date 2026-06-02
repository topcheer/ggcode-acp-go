package acp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	defaultQueueOwnerHeartbeatInterval = 5 * time.Second
	defaultQueueOwnerStaleAfter        = 15 * time.Second
)

func DefaultQueueOwnerHeartbeatInterval() time.Duration {
	return defaultQueueOwnerHeartbeatInterval
}

type QueueRequestStatus string

const (
	QueueRequestQueued    QueueRequestStatus = "queued"
	QueueRequestRunning   QueueRequestStatus = "running"
	QueueRequestCompleted QueueRequestStatus = "completed"
	QueueRequestFailed    QueueRequestStatus = "failed"
	QueueRequestCancelled QueueRequestStatus = "cancelled"
)

type QueueRequestKind string

const (
	QueueRequestPrompt    QueueRequestKind = "submit_prompt"
	QueueRequestSetMode   QueueRequestKind = "set_mode"
	QueueRequestSetConfig QueueRequestKind = "set_config_option"
	QueueRequestClose     QueueRequestKind = "close_session"
	QueueRequestSetModel  QueueRequestKind = "set_model"
)

// QueueRequestRecord is a durable queued prompt request.
type QueueRequestRecord struct {
	RequestID       string             `json:"requestId"`
	RecordID        string             `json:"recordId"`
	Kind            QueueRequestKind   `json:"kind,omitempty"`
	Prompt          string             `json:"prompt"`
	ModeID          string             `json:"modeId,omitempty"`
	ModelID         string             `json:"modelId,omitempty"`
	ConfigID        string             `json:"configId,omitempty"`
	ConfigValue     string             `json:"configValue,omitempty"`
	Status          QueueRequestStatus `json:"status"`
	CreatedAt       time.Time          `json:"createdAt"`
	StartedAt       *time.Time         `json:"startedAt,omitempty"`
	FinishedAt      *time.Time         `json:"finishedAt,omitempty"`
	CancelRequested bool               `json:"cancelRequested,omitempty"`
	ResultText      string             `json:"resultText,omitempty"`
	StopReason      string             `json:"stopReason,omitempty"`
	Error           string             `json:"error,omitempty"`
}

// QueueOwnerLease describes the current background owner process.
type QueueOwnerLease struct {
	RecordID        string    `json:"recordId"`
	PID             int       `json:"pid"`
	StartedAt       time.Time `json:"startedAt"`
	HeartbeatAt     time.Time `json:"heartbeatAt"`
	QueueDepth      int       `json:"queueDepth,omitempty"`
	SocketPath      string    `json:"socketPath,omitempty"`
	OwnerGeneration int64     `json:"ownerGeneration,omitempty"`
}

type QueueOwnerHealth struct {
	RecordID    string     `json:"recordId"`
	HasLease    bool       `json:"hasLease"`
	Healthy     bool       `json:"healthy"`
	PIDAlive    bool       `json:"pidAlive"`
	Stale       bool       `json:"stale"`
	PID         int        `json:"pid,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeatAt,omitempty"`
	QueueDepth  int        `json:"queueDepth,omitempty"`
}

func QueueOwnerLeaseAlive(lease *QueueOwnerLease) bool {
	return lease != nil && isPIDRunning(lease.PID) && !queueOwnerLeaseStale(lease, time.Now().UTC())
}

func queueOwnerLeaseStale(lease *QueueOwnerLease, now time.Time) bool {
	if lease == nil {
		return true
	}
	heartbeat := lease.HeartbeatAt
	if heartbeat.IsZero() {
		heartbeat = lease.StartedAt
	}
	if heartbeat.IsZero() {
		return true
	}
	return now.Sub(heartbeat) > defaultQueueOwnerStaleAfter
}

func newQueueOwnerGeneration() int64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return time.Now().UTC().UnixNano()
	}
	return int64(binary.BigEndian.Uint64(buf[:]) & ((1 << 48) - 1))
}

// QueueStore persists queued prompt requests and owner leases.
type QueueStore interface {
	Enqueue(recordID, prompt string) (*QueueRequestRecord, error)
	Load(recordID, requestID string) (*QueueRequestRecord, error)
	Save(*QueueRequestRecord) error
	List(recordID string) ([]*QueueRequestRecord, error)
	NextPending(recordID string) (*QueueRequestRecord, error)
	RequestCancel(recordID, requestID string) (*QueueRequestRecord, error)
	LoadLease(recordID string) (*QueueOwnerLease, error)
	SaveLease(*QueueOwnerLease) error
	ClearLease(recordID string) error
}

// FileQueueStore stores request records in JSON files under the state directory.
type FileQueueStore struct {
	stateDir string
}

func NewFileQueueStore(stateDir string) *FileQueueStore {
	return &FileQueueStore{stateDir: filepath.Clean(stateDir)}
}

func (s *FileQueueStore) queueRoot(recordID string) string {
	return filepath.Join(s.stateDir, "queue", safeRecordFileName(recordID))
}

func (s *FileQueueStore) requestsDir(recordID string) string {
	return filepath.Join(s.queueRoot(recordID), "requests")
}

func (s *FileQueueStore) ownersDir() string {
	return filepath.Join(s.stateDir, "queue", "owners")
}

func (s *FileQueueStore) requestPath(recordID, requestID string) string {
	return filepath.Join(s.requestsDir(recordID), safeRecordFileName(requestID)+".json")
}

func (s *FileQueueStore) requestMessagesPath(recordID, requestID string) string {
	return filepath.Join(s.requestsDir(recordID), safeRecordFileName(requestID)+".acp.ndjson")
}

func (s *FileQueueStore) leasePath(recordID string) string {
	return filepath.Join(s.ownersDir(), safeRecordFileName(recordID)+".json")
}

func (s *FileQueueStore) ensureRecordDir(recordID string) error {
	return os.MkdirAll(s.requestsDir(recordID), 0o755)
}

func (s *FileQueueStore) ensureOwnersDir() error {
	return os.MkdirAll(s.ownersDir(), 0o755)
}

func normalizeQueueRecord(record *QueueRequestRecord) *QueueRequestRecord {
	if record == nil {
		return nil
	}
	clone := *record
	if clone.RequestID == "" {
		clone.RequestID = generateSessionID()
	}
	if clone.Status == "" {
		clone.Status = QueueRequestQueued
	}
	if clone.Kind == "" {
		clone.Kind = QueueRequestPrompt
	}
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
	}
	return &clone
}

func (s *FileQueueStore) Enqueue(recordID, prompt string) (*QueueRequestRecord, error) {
	return s.enqueueRecord(&QueueRequestRecord{
		RecordID: recordID,
		Kind:     QueueRequestPrompt,
		Prompt:   prompt,
		Status:   QueueRequestQueued,
	})
}

func (s *FileQueueStore) EnqueueSetMode(recordID, modeID string) (*QueueRequestRecord, error) {
	return s.enqueueRecord(&QueueRequestRecord{
		RecordID: recordID,
		Kind:     QueueRequestSetMode,
		ModeID:   modeID,
		Status:   QueueRequestQueued,
	})
}

func (s *FileQueueStore) EnqueueSetConfig(recordID, configID, value string) (*QueueRequestRecord, error) {
	return s.enqueueRecord(&QueueRequestRecord{
		RecordID:    recordID,
		Kind:        QueueRequestSetConfig,
		ConfigID:    configID,
		ConfigValue: value,
		Status:      QueueRequestQueued,
	})
}

func (s *FileQueueStore) EnqueueSetModel(recordID, modelID string) (*QueueRequestRecord, error) {
	return s.enqueueRecord(&QueueRequestRecord{
		RecordID: recordID,
		Kind:     QueueRequestSetModel,
		ModelID:  modelID,
		Status:   QueueRequestQueued,
	})
}

func (s *FileQueueStore) EnqueueClose(recordID string) (*QueueRequestRecord, error) {
	return s.enqueueRecord(&QueueRequestRecord{
		RecordID: recordID,
		Kind:     QueueRequestClose,
		Status:   QueueRequestQueued,
	})
}

func (s *FileQueueStore) enqueueRecord(record *QueueRequestRecord) (*QueueRequestRecord, error) {
	record = normalizeQueueRecord(record)
	if err := s.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *FileQueueStore) Load(recordID, requestID string) (*QueueRequestRecord, error) {
	if err := s.ensureRecordDir(recordID); err != nil {
		return nil, fmt.Errorf("creating queue request directory: %w", err)
	}
	payload, err := os.ReadFile(s.requestPath(recordID, requestID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading queue request %q: %w", requestID, err)
	}
	var record QueueRequestRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, fmt.Errorf("decoding queue request %q: %w", requestID, err)
	}
	return normalizeQueueRecord(&record), nil
}

func (s *FileQueueStore) Save(record *QueueRequestRecord) error {
	record = normalizeQueueRecord(record)
	if record == nil {
		return fmt.Errorf("queue request record is nil")
	}
	if record.RecordID == "" {
		return fmt.Errorf("queue request missing record id")
	}
	if err := s.ensureRecordDir(record.RecordID); err != nil {
		return fmt.Errorf("creating queue request directory: %w", err)
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding queue request %q: %w", record.RequestID, err)
	}
	path := s.requestPath(record.RecordID, record.RequestID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing queue request %q: %w", record.RequestID, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("committing queue request %q: %w", record.RequestID, err)
	}
	return nil
}

func (s *FileQueueStore) AppendRawMessage(recordID, requestID string, message json.RawMessage) error {
	if len(message) == 0 {
		return nil
	}
	if err := s.ensureRecordDir(recordID); err != nil {
		return fmt.Errorf("creating queue request directory: %w", err)
	}
	fh, err := os.OpenFile(s.requestMessagesPath(recordID, requestID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening queue raw message log %q: %w", requestID, err)
	}
	defer fh.Close()
	if _, err := fh.Write(message); err != nil {
		return fmt.Errorf("writing queue raw message %q: %w", requestID, err)
	}
	if _, err := fh.Write([]byte("\n")); err != nil {
		return fmt.Errorf("writing queue raw message newline %q: %w", requestID, err)
	}
	return nil
}

func (s *FileQueueStore) ReadRawMessages(recordID, requestID string) ([][]byte, error) {
	if err := s.ensureRecordDir(recordID); err != nil {
		return nil, fmt.Errorf("creating queue request directory: %w", err)
	}
	payload, err := os.ReadFile(s.requestMessagesPath(recordID, requestID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading queue raw message log %q: %w", requestID, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	lines := make([][]byte, 0)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning queue raw message log %q: %w", requestID, err)
	}
	return lines, nil
}

func (s *FileQueueStore) List(recordID string) ([]*QueueRequestRecord, error) {
	if err := s.ensureRecordDir(recordID); err != nil {
		return nil, fmt.Errorf("creating queue request directory: %w", err)
	}
	entries, err := os.ReadDir(s.requestsDir(recordID))
	if err != nil {
		return nil, fmt.Errorf("listing queue requests: %w", err)
	}
	requests := make([]*QueueRequestRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(s.requestsDir(recordID), entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading queue request %q: %w", entry.Name(), err)
		}
		var record QueueRequestRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, fmt.Errorf("decoding queue request %q: %w", entry.Name(), err)
		}
		requests = append(requests, normalizeQueueRecord(&record))
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	return requests, nil
}

func (s *FileQueueStore) NextPending(recordID string) (*QueueRequestRecord, error) {
	requests, err := s.List(recordID)
	if err != nil {
		return nil, err
	}
	for _, record := range requests {
		if record.Status == QueueRequestQueued {
			return record, nil
		}
	}
	return nil, nil
}

func (s *FileQueueStore) RequestCancel(recordID, requestID string) (*QueueRequestRecord, error) {
	record, err := s.Load(recordID, requestID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	record.CancelRequested = true
	if err := s.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *FileQueueStore) LoadLease(recordID string) (*QueueOwnerLease, error) {
	if err := s.ensureOwnersDir(); err != nil {
		return nil, fmt.Errorf("creating owner lease directory: %w", err)
	}
	payload, err := os.ReadFile(s.leasePath(recordID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading owner lease %q: %w", recordID, err)
	}
	var lease QueueOwnerLease
	if err := json.Unmarshal(payload, &lease); err != nil {
		return nil, fmt.Errorf("decoding owner lease %q: %w", recordID, err)
	}
	if lease.RecordID == "" {
		lease.RecordID = recordID
	}
	if lease.HeartbeatAt.IsZero() {
		lease.HeartbeatAt = lease.StartedAt
	}
	return &lease, nil
}

func (s *FileQueueStore) SaveLease(lease *QueueOwnerLease) error {
	if lease == nil {
		return fmt.Errorf("owner lease is nil")
	}
	if err := s.ensureOwnersDir(); err != nil {
		return fmt.Errorf("creating owner lease directory: %w", err)
	}
	if lease.StartedAt.IsZero() {
		lease.StartedAt = time.Now().UTC()
	}
	if lease.HeartbeatAt.IsZero() {
		lease.HeartbeatAt = lease.StartedAt
	}
	if lease.QueueDepth < 0 {
		lease.QueueDepth = 0
	}
	if lease.OwnerGeneration == 0 {
		lease.OwnerGeneration = newQueueOwnerGeneration()
	}
	payload, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding owner lease %q: %w", lease.RecordID, err)
	}
	path := s.leasePath(lease.RecordID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing owner lease %q: %w", lease.RecordID, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("committing owner lease %q: %w", lease.RecordID, err)
	}
	return nil
}

func (s *FileQueueStore) TryAcquireLease(lease *QueueOwnerLease) (bool, error) {
	if lease == nil {
		return false, fmt.Errorf("owner lease is nil")
	}
	if err := s.ensureOwnersDir(); err != nil {
		return false, fmt.Errorf("creating owner lease directory: %w", err)
	}
	if lease.StartedAt.IsZero() {
		lease.StartedAt = time.Now().UTC()
	}
	if lease.HeartbeatAt.IsZero() {
		lease.HeartbeatAt = lease.StartedAt
	}
	if lease.QueueDepth < 0 {
		lease.QueueDepth = 0
	}
	if lease.OwnerGeneration == 0 {
		lease.OwnerGeneration = newQueueOwnerGeneration()
	}
	payload, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding owner lease %q: %w", lease.RecordID, err)
	}
	path := s.leasePath(lease.RecordID)
	for attempt := 0; attempt < 2; attempt++ {
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if _, writeErr := fh.Write(append(payload, '\n')); writeErr != nil {
				_ = fh.Close()
				_ = os.Remove(path)
				return false, fmt.Errorf("writing owner lease %q: %w", lease.RecordID, writeErr)
			}
			if closeErr := fh.Close(); closeErr != nil {
				_ = os.Remove(path)
				return false, fmt.Errorf("closing owner lease %q: %w", lease.RecordID, closeErr)
			}
			return true, nil
		}
		if !os.IsExist(err) {
			return false, fmt.Errorf("acquiring owner lease %q: %w", lease.RecordID, err)
		}
		existing, loadErr := s.LoadLease(lease.RecordID)
		if loadErr != nil {
			return false, loadErr
		}
		if QueueOwnerLeaseAlive(existing) {
			return false, nil
		}
		if clearErr := s.ClearLease(lease.RecordID); clearErr != nil {
			return false, clearErr
		}
	}
	return false, nil
}

func (s *FileQueueStore) ClearLease(recordID string) error {
	if err := os.Remove(s.leasePath(recordID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing owner lease %q: %w", recordID, err)
	}
	return nil
}

func (s *FileQueueStore) RequeueOrphanedRunningRequests(recordID string) ([]*QueueRequestRecord, error) {
	requests, err := s.List(recordID)
	if err != nil {
		return nil, err
	}
	reset := make([]*QueueRequestRecord, 0)
	for _, record := range requests {
		if record.Status != QueueRequestRunning {
			continue
		}
		record.Status = QueueRequestQueued
		record.StartedAt = nil
		record.FinishedAt = nil
		record.CancelRequested = false
		record.ResultText = ""
		record.StopReason = ""
		record.Error = ""
		if err := s.Save(record); err != nil {
			return nil, err
		}
		reset = append(reset, record)
	}
	return reset, nil
}

func (s *FileQueueStore) RefreshLease(recordID string, pid, queueDepth int) (*QueueOwnerLease, error) {
	lease, err := s.LoadLease(recordID)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		lease = &QueueOwnerLease{RecordID: recordID, PID: pid}
	}
	lease.PID = pid
	lease.QueueDepth = queueDepth
	lease.HeartbeatAt = time.Now().UTC()
	if err := s.SaveLease(lease); err != nil {
		return nil, err
	}
	return lease, nil
}

func (s *FileQueueStore) ProbeOwner(recordID string) (*QueueOwnerHealth, error) {
	lease, err := s.LoadLease(recordID)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return &QueueOwnerHealth{
			RecordID: recordID,
			HasLease: false,
		}, nil
	}
	now := time.Now().UTC()
	alive := isPIDRunning(lease.PID)
	stale := queueOwnerLeaseStale(lease, now)
	if !alive || stale {
		if err := s.ClearLease(recordID); err != nil {
			return nil, err
		}
		return &QueueOwnerHealth{
			RecordID: recordID,
			HasLease: false,
			Healthy:  false,
			PIDAlive: alive,
			Stale:    stale,
			PID:      lease.PID,
			StartedAt: func() *time.Time {
				if lease.StartedAt.IsZero() {
					return nil
				}
				t := lease.StartedAt
				return &t
			}(),
			HeartbeatAt: func() *time.Time {
				if lease.HeartbeatAt.IsZero() {
					return nil
				}
				t := lease.HeartbeatAt
				return &t
			}(),
			QueueDepth: lease.QueueDepth,
		}, nil
	}
	startedAt := lease.StartedAt
	heartbeatAt := lease.HeartbeatAt
	return &QueueOwnerHealth{
		RecordID:    recordID,
		HasLease:    true,
		Healthy:     true,
		PIDAlive:    true,
		Stale:       false,
		PID:         lease.PID,
		StartedAt:   &startedAt,
		HeartbeatAt: &heartbeatAt,
		QueueDepth:  lease.QueueDepth,
	}, nil
}
