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
)

var defaultActiveRequestTracker = newActiveRequestTracker()

const (
	activeRequestRetryTriggered              = "retried"
	activeRequestRetryIgnoredFinished        = "ignored_finished"
	activeRequestRetryIgnoredResponseStarted = "ignored_response_started"
	activeRequestRetryIgnoredUnauthorized    = "ignored_unauthorized"
)

type activeRequestTracker struct {
	mu       sync.RWMutex
	nextID   int64
	requests map[int64]activeRequestSnapshot
}

type activeRequestSnapshot struct {
	startedAt       time.Time
	log             ReqeustLog
	cancel          context.CancelFunc
	retryRequested  bool
	responseStarted bool
}

type ActiveRequestRetryResult struct {
	Status string `json:"status"`
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
	next.retryRequested = existing.retryRequested
	next.responseStarted = existing.responseStarted
	if next.retryRequested {
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

func (t *activeRequestTracker) RegisterCancel(id int64, cancel context.CancelFunc) {
	if t == nil || id == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return
	}
	existing.cancel = cancel
	t.requests[id] = existing
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
	if existing.responseStarted {
		t.mu.Unlock()
		return ActiveRequestRetryResult{Status: activeRequestRetryIgnoredResponseStarted}
	}
	existing.retryRequested = true
	existing.log.RetryRequested = true
	existing.log.Status = requestLogStatusRetrying
	cancel := existing.cancel
	t.requests[id] = existing
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return ActiveRequestRetryResult{Status: activeRequestRetryTriggered}
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
	snapshot.Status = requestLogStatusProcessing
	return activeRequestSnapshot{
		startedAt: startedAt,
		log:       snapshot,
	}
}
