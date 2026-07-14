package services

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	requestLogStatusCompleted  = "completed"
	requestLogStatusProcessing = "processing"
	requestLogStatusRetrying   = "retrying"
	requestLogStatusQueued     = "queued"
)

var defaultActiveRequestTracker = newActiveRequestTracker()

const (
	activeRequestRetryTriggered              = "retried"
	activeRequestRetryIgnoredFinished        = "ignored_finished"
	activeRequestRetryIgnoredFirstText       = "ignored_first_text"
	activeRequestRetryIgnoredResponseStarted = "ignored_response_started"
	activeRequestRetryIgnoredUnauthorized    = "ignored_unauthorized"
	activeRequestRetryIgnoredQueued          = "ignored_queued"
	activeRequestRetryIgnoredTransition      = "ignored_transition"
)

type activeRequestTracker struct {
	mu       sync.RWMutex
	nextID   int64
	requests map[int64]activeRequestSnapshot
}

type activeRequestSnapshot struct {
	startedAt        time.Time
	log              ReqeustLog
	cancel           context.CancelFunc
	cancelGeneration uint64
	retryRequested   bool
	responseStarted  bool
}

type ActiveRequestRetryResult struct {
	Status                string  `json:"status"`
	FirstTokenDurationSec float64 `json:"first_token_duration_sec,omitempty"`
	FirstTextSec          float64 `json:"first_text_sec,omitempty"`
}

func newActiveRequestTracker() *activeRequestTracker {
	return &activeRequestTracker{
		requests: make(map[int64]activeRequestSnapshot),
	}
}

func (t *activeRequestTracker) Start(logEntry *ReqeustLog, startedAt time.Time) int64 {
	if t == nil || logEntry == nil {
		return 0
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.nextID++
	id := t.nextID
	t.requests[id] = snapshotActiveRequest(id, logEntry, startedAt)
	return id
}

func (t *activeRequestTracker) Update(id int64, logEntry *ReqeustLog) {
	if t == nil || id == 0 || logEntry == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return
	}
	next := snapshotActiveRequest(id, logEntry, existing.startedAt)
	next.cancel = existing.cancel
	next.cancelGeneration = existing.cancelGeneration
	next.retryRequested = existing.retryRequested
	next.responseStarted = existing.responseStarted
	if next.retryRequested && next.log.Status != requestLogStatusQueued {
		next.log.RetryRequested = true
		next.log.Status = requestLogStatusRetrying
	}
	t.requests[id] = next
}

func (t *activeRequestTracker) Finish(id int64) {
	if t == nil || id == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.requests, id)
}

func (t *activeRequestTracker) RegisterCancel(id int64, cancel context.CancelFunc) uint64 {
	if t == nil || id == 0 || cancel == nil {
		return 0
	}

	t.mu.Lock()
	existing, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return 0
	}
	existing.cancelGeneration++
	generation := existing.cancelGeneration
	existing.cancel = cancel
	t.requests[id] = existing
	t.mu.Unlock()
	return generation
}

// BeginAttempt makes a new upstream attempt retryable only after its state and
// cancellation function have been installed together. Keeping this transition
// under one lock prevents the Logs retry action from observing a processing
// request that has no cancellation function yet.
func (t *activeRequestTracker) BeginAttempt(id int64, logEntry *ReqeustLog, cancel context.CancelFunc) uint64 {
	if t == nil || id == 0 || logEntry == nil || cancel == nil {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return 0
	}
	attemptStartedAt := logEntry.startedAt
	if attemptStartedAt.IsZero() {
		attemptStartedAt = existing.startedAt
	}
	next := snapshotActiveRequest(id, logEntry, attemptStartedAt)
	next.cancelGeneration = existing.cancelGeneration + 1
	next.cancel = cancel
	next.retryRequested = false
	next.responseStarted = false
	next.log.Status = requestLogStatusProcessing
	next.log.RetryRequested = false
	t.requests[id] = next
	return next.cancelGeneration
}

// ResetAttemptStart keeps the logical request ID stable while making timing in
// the Logs page relative to the provider/key attempt that is actually starting.
func (t *activeRequestTracker) ResetAttemptStart(id int64, logEntry *ReqeustLog, startedAt time.Time) {
	if t == nil || id == 0 || logEntry == nil {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	existing, ok := t.requests[id]
	if !ok {
		return
	}
	next := snapshotActiveRequest(id, logEntry, startedAt)
	next.cancel = existing.cancel
	next.cancelGeneration = existing.cancelGeneration
	next.retryRequested = existing.retryRequested
	next.responseStarted = existing.responseStarted
	t.requests[id] = next
}

// MarkAttemptTransition removes a completed provider/key from the active row.
// Its 504 snapshot is already persisted; the remaining active row represents
// selection or waiting for the next attempt and starts timing from zero.
func (t *activeRequestTracker) MarkAttemptTransition(id int64, startedAt time.Time) {
	if t == nil || id == 0 {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	existing, ok := t.requests[id]
	if !ok {
		return
	}
	existing.startedAt = startedAt
	existing.log.Provider = ""
	existing.log.HttpCode = 0
	existing.log.InputTokens = 0
	existing.log.OutputTokens = 0
	existing.log.CacheCreateTokens = 0
	existing.log.CacheReadTokens = 0
	existing.log.ReasoningTokens = 0
	existing.log.UpstreamHeaderSec = 0
	existing.log.FirstEventSec = 0
	existing.log.FirstTextSec = 0
	existing.log.FirstTokenDurationSec = 0
	existing.log.ErrorMessage = "重试"
	existing.log.Status = requestLogStatusRetrying
	existing.log.RetryRequested = true
	existing.log.CreatedAt = startedAt.In(beijingLocation).Format(timeLayout)
	existing.retryRequested = false
	existing.responseStarted = false
	existing.cancel = nil
	t.requests[id] = existing
}

func (t *activeRequestTracker) UnregisterCancel(id int64, generation uint64) bool {
	if t == nil || id == 0 || generation == 0 {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok || existing.cancelGeneration != generation {
		return false
	}
	existing.cancel = nil
	t.requests[id] = existing
	return existing.retryRequested
}

func (t *activeRequestTracker) MarkResponseStarted(id int64) {
	if t == nil || id == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return
	}
	existing.responseStarted = true
	t.requests[id] = existing
}

func (t *activeRequestTracker) MarkQueued(id int64, queueKey string, position int) {
	if t == nil || id == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return
	}
	if existing.log.QueueStartedAt == "" {
		existing.log.QueueStartedAt = time.Now().In(beijingLocation).Format(timeLayout)
	}
	existing.log.Status = requestLogStatusQueued
	existing.log.Provider = ""
	existing.log.QueueKey = queueKey
	existing.log.QueuePosition = position
	existing.log.ErrorMessage = "排队中"
	existing.log.RetryRequested = false
	existing.retryRequested = false
	existing.responseStarted = false
	t.requests[id] = existing
}

func (t *activeRequestTracker) MarkProcessing(id int64, provider string) {
	if t == nil || id == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return
	}
	existing.log.Status = requestLogStatusProcessing
	existing.log.Provider = provider
	existing.log.QueueKey = ""
	existing.log.QueuePosition = 0
	existing.log.QueueStartedAt = ""
	existing.log.ErrorMessage = ""
	existing.log.RetryRequested = false
	existing.retryRequested = false
	existing.responseStarted = false
	t.requests[id] = existing
}

func (t *activeRequestTracker) UpdateQueuePositions(queueKey string, positions map[int64]int) {
	if t == nil || strings.TrimSpace(queueKey) == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for id, existing := range t.requests {
		if existing.log.Status != requestLogStatusQueued || existing.log.QueueKey != queueKey {
			continue
		}
		position, ok := positions[id]
		if !ok {
			continue
		}
		existing.log.QueuePosition = position
		t.requests[id] = existing
	}
}

func (t *activeRequestTracker) IsRetryRequested(id int64) bool {
	if t == nil || id == 0 {
		return false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	existing, ok := t.requests[id]
	return ok && existing.retryRequested
}

func (t *activeRequestTracker) Retry(id int64, userID string) ActiveRequestRetryResult {
	if t == nil || id == 0 {
		return ActiveRequestRetryResult{Status: activeRequestRetryIgnoredFinished}
	}
	if id < 0 {
		id = -id
	}

	t.mu.Lock()
	existing, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return ActiveRequestRetryResult{Status: activeRequestRetryIgnoredFinished}
	}
	userID = strings.TrimSpace(userID)
	if userID != "" && strings.TrimSpace(existing.log.UserID) != userID {
		t.mu.Unlock()
		return ActiveRequestRetryResult{Status: activeRequestRetryIgnoredUnauthorized}
	}
	if activeRequestHasFirstText(existing.log) {
		firstTokenSec := existing.log.FirstTokenDurationSec
		firstTextSec := existing.log.FirstTextSec
		t.mu.Unlock()
		return ActiveRequestRetryResult{
			Status:                activeRequestRetryIgnoredFirstText,
			FirstTokenDurationSec: firstTokenSec,
			FirstTextSec:          firstTextSec,
		}
	}
	if existing.log.Status == requestLogStatusQueued {
		t.mu.Unlock()
		return ActiveRequestRetryResult{Status: activeRequestRetryIgnoredQueued}
	}
	if existing.responseStarted {
		firstTokenSec := existing.log.FirstTokenDurationSec
		firstTextSec := existing.log.FirstTextSec
		t.mu.Unlock()
		return ActiveRequestRetryResult{
			Status:                activeRequestRetryIgnoredResponseStarted,
			FirstTokenDurationSec: firstTokenSec,
			FirstTextSec:          firstTextSec,
		}
	}
	if existing.retryRequested {
		t.mu.Unlock()
		return ActiveRequestRetryResult{Status: activeRequestRetryTriggered}
	}
	if existing.cancel == nil {
		t.mu.Unlock()
		return ActiveRequestRetryResult{Status: activeRequestRetryIgnoredTransition}
	}
	existing.retryRequested = true
	existing.log.RetryRequested = true
	existing.log.Status = requestLogStatusRetrying
	cancel := existing.cancel
	t.requests[id] = existing
	t.mu.Unlock()

	cancel()
	return ActiveRequestRetryResult{Status: activeRequestRetryTriggered}
}

func activeRequestHasFirstText(logEntry ReqeustLog) bool {
	return logEntry.FirstTextSec > 0 || logEntry.FirstTokenDurationSec > 0
}

func (t *activeRequestTracker) List(platform, provider, userID string) []ReqeustLog {
	if t == nil {
		return nil
	}

	now := time.Now()
	platform = strings.TrimSpace(platform)
	provider = strings.TrimSpace(provider)
	userID = strings.TrimSpace(userID)

	t.mu.RLock()
	snapshots := make([]activeRequestSnapshot, 0, len(t.requests))
	for _, snapshot := range t.requests {
		if platform != "" && snapshot.log.Platform != platform {
			continue
		}
		if provider != "" && snapshot.log.Provider != provider {
			continue
		}
		if userID != "" && snapshot.log.UserID != userID {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	t.mu.RUnlock()

	sort.SliceStable(snapshots, func(i, j int) bool {
		return snapshots[i].startedAt.After(snapshots[j].startedAt)
	})

	logs := make([]ReqeustLog, 0, len(snapshots))
	for _, snapshot := range snapshots {
		logEntry := snapshot.log
		logEntry.DurationSec = now.Sub(snapshot.startedAt).Seconds()
		if logEntry.DurationSec < 0 {
			logEntry.DurationSec = 0
		}
		if snapshot.retryRequested {
			if logEntry.Status == requestLogStatusQueued {
				logs = append(logs, logEntry)
				continue
			}
			logEntry.RetryRequested = true
			logEntry.Status = requestLogStatusRetrying
		}
		logs = append(logs, logEntry)
	}
	return logs
}

func snapshotActiveRequest(id int64, logEntry *ReqeustLog, startedAt time.Time) activeRequestSnapshot {
	snapshot := *logEntry
	snapshot.ID = -id
	snapshot.HttpCode = 0
	snapshot.DurationSec = 0
	snapshot.CreatedAt = startedAt.In(beijingLocation).Format(timeLayout)
	if snapshot.Status == "" {
		snapshot.Status = requestLogStatusProcessing
	}
	return activeRequestSnapshot{
		startedAt: startedAt,
		log:       snapshot,
	}
}
