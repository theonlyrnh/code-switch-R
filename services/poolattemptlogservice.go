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
	mu   sync.RWMutex
	logs map[string][]ConsoleLog
}

func NewPoolAttemptLogService() *PoolAttemptLogService {
	return &PoolAttemptLogService{logs: make(map[string][]ConsoleLog)}
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
	s.logs[userID] = append(s.logs[userID], log)
	if len(s.logs[userID]) > 1000 {
		s.logs[userID] = append([]ConsoleLog(nil), s.logs[userID][len(s.logs[userID])-1000:]...)
	}
}

func (s *PoolAttemptLogService) List(userID string, limit int, since time.Time) []ConsoleLog {
	if s == nil || strings.TrimSpace(userID) == "" {
		return []ConsoleLog{}
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	s.mu.RLock()
	entries := append([]ConsoleLog(nil), s.logs[userID]...)
	s.mu.RUnlock()
	filtered := entries[:0]
	for _, entry := range entries {
		if !since.IsZero() && !entry.Timestamp.After(since) {
			continue
		}
		filtered = append(filtered, entry)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Timestamp.Before(filtered[j].Timestamp) })
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered
}
