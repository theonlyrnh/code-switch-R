package services

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// PoolAttemptLogService keeps transient upstream-attempt errors separate from
// request_log. They are intentionally not part of request statistics.
type PoolAttemptLogService struct {
	mu           sync.RWMutex
	logs         map[string][]ConsoleLog
	nextSequence map[string]uint64
}

func NewPoolAttemptLogService() *PoolAttemptLogService {
	return &PoolAttemptLogService{
		logs:         make(map[string][]ConsoleLog),
		nextSequence: make(map[string]uint64),
	}
}

func (s *PoolAttemptLogService) Add(userID string, log ConsoleLog) {
	if s == nil || strings.TrimSpace(userID) == "" {
		return
	}
	if log.Timestamp.IsZero() {
		log.Timestamp = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs == nil {
		s.logs = make(map[string][]ConsoleLog)
	}
	if s.nextSequence == nil {
		s.nextSequence = make(map[string]uint64)
	}
	s.nextSequence[userID]++
	log.sequence = s.nextSequence[userID]
	s.logs[userID] = append(s.logs[userID], log)
	if len(s.logs[userID]) > 1000 {
		s.logs[userID] = append([]ConsoleLog(nil), s.logs[userID][len(s.logs[userID])-1000:]...)
	}
}

func (s *PoolAttemptLogService) List(userID string, limit int, since time.Time) []ConsoleLog {
	logs, _ := s.ListAfter(userID, 0, limit, since)
	return logs
}

// ListAfter returns the newest bounded entries after a per-user sequence. The
// returned cursor advances to the source snapshot even when old entries were
// filtered by a clear time or truncated by the result limit.
func (s *PoolAttemptLogService) ListAfter(userID string, after uint64, limit int, since time.Time) ([]ConsoleLog, uint64) {
	if s == nil || strings.TrimSpace(userID) == "" {
		return []ConsoleLog{}, after
	}
	limit = normalizeConsoleLogLimit(limit)
	s.mu.RLock()
	entries := append([]ConsoleLog(nil), s.logs[userID]...)
	next := s.nextSequence[userID]
	s.mu.RUnlock()
	filtered := entries[:0]
	for _, entry := range entries {
		if entry.sequence <= after {
			continue
		}
		if !since.IsZero() && !entry.Timestamp.After(since) {
			continue
		}
		filtered = append(filtered, entry)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Timestamp.Before(filtered[j].Timestamp) })
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, next
}
