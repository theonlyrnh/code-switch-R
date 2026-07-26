package services

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIPFromRequest(t *testing.T) {
	t.Setenv(relayTrustedProxiesEnv, "")

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	if got := clientIPFromRequest(req); got != "127.0.0.1" {
		t.Fatalf("clientIPFromRequest = %q, want 127.0.0.1", got)
	}

	req.RemoteAddr = "[::1]:12345"
	if got := clientIPFromRequest(req); got != "::1" {
		t.Fatalf("clientIPFromRequest IPv6 = %q, want ::1", got)
	}

	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.25, 10.0.0.2")
	if got := clientIPFromRequest(req); got != "203.0.113.25" {
		t.Fatalf("clientIPFromRequest trusted X-Forwarded-For = %q, want 203.0.113.25", got)
	}

	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "198.51.100.7")
	if got := clientIPFromRequest(req); got != "198.51.100.7" {
		t.Fatalf("clientIPFromRequest trusted X-Real-IP = %q, want 198.51.100.7", got)
	}

	req.RemoteAddr = "198.51.100.10:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	if got := clientIPFromRequest(req); got != "198.51.100.10" {
		t.Fatalf("clientIPFromRequest untrusted proxy = %q, want 198.51.100.10", got)
	}
}

func TestClientIPFromRequestUsesConfiguredTrustedProxy(t *testing.T) {
	t.Setenv(relayTrustedProxiesEnv, "10.0.0.0/8")

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.RemoteAddr = "10.1.2.3:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.30")

	if got := clientIPFromRequest(req); got != "203.0.113.30" {
		t.Fatalf("clientIPFromRequest configured trusted proxy = %q, want 203.0.113.30", got)
	}
}

func TestActiveRequestTrackerListFiltersAndFinishes(t *testing.T) {
	tracker := newActiveRequestTracker()
	now := time.Now()

	oldID := tracker.Start(&ReqeustLog{
		UserID:     "user-a",
		Platform:   "claude",
		Provider:   "provider-a",
		Model:      "claude-sonnet",
		IsStream:   true,
		ClientIP:   "127.0.0.1",
		RelayKeyID: "key-a",
	}, now.Add(-3*time.Second))
	newID := tracker.Start(&ReqeustLog{
		UserID:   "user-b",
		Platform: "openai-responses",
		Provider: "provider-b",
		Model:    "gpt-5",
	}, now.Add(-time.Second))

	logs := tracker.List("", "", "")
	if len(logs) != 2 {
		t.Fatalf("active logs count = %d, want 2", len(logs))
	}
	if logs[0].Provider != "provider-b" || logs[1].Provider != "provider-a" {
		t.Fatalf("active logs order = [%s, %s], want newest first", logs[0].Provider, logs[1].Provider)
	}
	if logs[0].ID >= 0 || logs[0].Status != requestLogStatusProcessing {
		t.Fatalf("active log metadata = id %d status %q, want negative processing", logs[0].ID, logs[0].Status)
	}
	if logs[1].DurationSec <= 0 {
		t.Fatalf("active duration = %f, want positive", logs[1].DurationSec)
	}

	tracker.Update(oldID, &ReqeustLog{
		UserID:   "user-a",
		Platform: "claude",
		Provider: "provider-c",
		Model:    "claude-opus",
	})
	filtered := tracker.List("claude", "provider-c", "user-a")
	if len(filtered) != 1 || filtered[0].Model != "claude-opus" {
		t.Fatalf("filtered active logs = %#v, want updated claude provider-c", filtered)
	}

	tracker.Finish(oldID)
	tracker.Finish(newID)
	if remaining := tracker.List("", "", ""); len(remaining) != 0 {
		t.Fatalf("remaining active logs = %d, want 0", len(remaining))
	}
}

func TestActiveRequestTrackerBeginAttemptResetsVisibleDuration(t *testing.T) {
	tracker := newActiveRequestTracker()
	requestStartedAt := time.Now().Add(-30 * time.Second)
	requestLog := &ReqeustLog{
		UserID:    "user-a",
		Platform:  "openai-responses",
		Provider:  "key-a",
		Model:     "gpt-5",
		startedAt: requestStartedAt,
	}
	id := tracker.Start(requestLog, requestStartedAt)

	attemptStartedAt := time.Now()
	requestLog.startedAt = attemptStartedAt
	requestLog.Provider = "key-b"
	tracker.BeginAttempt(id, requestLog, func() {})

	logs := tracker.List("openai-responses", "", "user-a")
	if len(logs) != 1 {
		t.Fatalf("active logs = %+v, want one fresh attempt", logs)
	}
	if logs[0].Provider != "key-b" || logs[0].DurationSec < 0 || logs[0].DurationSec >= time.Second.Seconds() {
		t.Fatalf("fresh attempt log = %+v, want key-b duration below one second", logs[0])
	}
}

func TestActiveRequestTrackerTimeoutTransitionDropsOldKeyAndResetsDuration(t *testing.T) {
	tracker := newActiveRequestTracker()
	startedAt := time.Now().Add(-5 * time.Second)
	requestLog := &ReqeustLog{
		UserID:       "user-a",
		Platform:     "openai-responses",
		Provider:     "timed-out-key",
		Model:        "gpt-5",
		HttpCode:     504,
		ErrorMessage: firstTextTimeoutErrorBody,
	}
	id := tracker.Start(requestLog, startedAt)
	tracker.MarkAttemptTransition(id, time.Now())

	logs := tracker.List("openai-responses", "", "user-a")
	if len(logs) != 1 {
		t.Fatalf("transition logs = %+v, want one", logs)
	}
	if logs[0].Provider != "" || logs[0].Status != requestLogStatusRetrying || logs[0].HttpCode != 0 {
		t.Fatalf("transition log = %+v, want provider-free retrying row", logs[0])
	}
	if logs[0].DurationSec < 0 || logs[0].DurationSec >= time.Second.Seconds() {
		t.Fatalf("transition duration = %.3fs, want below one second", logs[0].DurationSec)
	}
}

func TestActiveRequestTrackerRetryCancelsAndMarksLog(t *testing.T) {
	tracker := newActiveRequestTracker()
	ctx, cancel := context.WithCancel(context.Background())
	activeID := tracker.Start(&ReqeustLog{
		UserID:   "user-a",
		Platform: "claude",
		Provider: "provider-a",
		Model:    "claude-sonnet",
	}, time.Now())
	tracker.RegisterCancel(activeID, cancel)

	result := tracker.Retry(-activeID, "user-a")
	if result.Status != activeRequestRetryTriggered {
		t.Fatalf("retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}
	if ctx.Err() == nil {
		t.Fatalf("retry did not cancel request context")
	}
	if !tracker.IsRetryRequested(activeID) {
		t.Fatalf("retry requested flag = false, want true")
	}
	logs := tracker.List("", "", "user-a")
	if len(logs) != 1 || logs[0].Status != requestLogStatusRetrying || !logs[0].RetryRequested {
		t.Fatalf("retry log = %#v, want retrying active log", logs)
	}
}

func TestActiveRequestTrackerRetryIsIdempotentForCurrentAttempt(t *testing.T) {
	tracker := newActiveRequestTracker()
	activeID := tracker.Start(&ReqeustLog{UserID: "user-a", Provider: "provider-a"}, time.Now())
	cancelCalls := 0
	tracker.RegisterCancel(activeID, func() { cancelCalls++ })

	for i := 0; i < 2; i++ {
		if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryTriggered {
			t.Fatalf("retry %d status = %q, want %q", i+1, result.Status, activeRequestRetryTriggered)
		}
	}
	if cancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want one for repeated retry", cancelCalls)
	}
}

func TestActiveRequestTrackerBeginAttemptInstallsRetryStateAtomically(t *testing.T) {
	tracker := newActiveRequestTracker()
	activeID := tracker.Start(&ReqeustLog{UserID: "user-a", Provider: "provider-a"}, time.Now())

	firstCancelCalls := 0
	if generation := tracker.BeginAttempt(activeID, &ReqeustLog{UserID: "user-a", Provider: "provider-a"}, func() { firstCancelCalls++ }); generation == 0 {
		t.Fatal("first attempt generation = 0")
	}
	if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryTriggered {
		t.Fatalf("first retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}
	if firstCancelCalls != 1 {
		t.Fatalf("first cancel calls = %d, want 1", firstCancelCalls)
	}

	secondCancelCalls := 0
	if generation := tracker.BeginAttempt(activeID, &ReqeustLog{UserID: "user-a", Provider: "provider-a"}, func() { secondCancelCalls++ }); generation == 0 {
		t.Fatal("second attempt generation = 0")
	}
	if tracker.IsRetryRequested(activeID) {
		t.Fatal("new attempt retained a completed retry request")
	}
	if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryTriggered {
		t.Fatalf("second retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}
	if secondCancelCalls != 1 {
		t.Fatalf("second cancel calls = %d, want 1", secondCancelCalls)
	}
}

func TestActiveRequestTrackerCancelRegistrationGeneration(t *testing.T) {
	tracker := newActiveRequestTracker()
	activeID := tracker.Start(&ReqeustLog{UserID: "user-a", Provider: "provider-a"}, time.Now())
	firstCancelCalls := 0
	secondCancelCalls := 0
	firstGeneration := tracker.RegisterCancel(activeID, func() { firstCancelCalls++ })
	secondGeneration := tracker.RegisterCancel(activeID, func() { secondCancelCalls++ })

	tracker.UnregisterCancel(activeID, firstGeneration)
	if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryTriggered {
		t.Fatalf("retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}
	if firstCancelCalls != 0 || secondCancelCalls != 1 {
		t.Fatalf("cancel calls = (%d, %d), want (0, 1)", firstCancelCalls, secondCancelCalls)
	}

	tracker.UnregisterCancel(activeID, secondGeneration)
}

func TestActiveRequestTrackerRetryDuringAttemptGapIsRejected(t *testing.T) {
	tracker := newActiveRequestTracker()
	activeID := tracker.Start(&ReqeustLog{UserID: "user-a", Provider: "provider-a"}, time.Now())
	staleCancelCalls := 0
	staleGeneration := tracker.RegisterCancel(activeID, func() { staleCancelCalls++ })
	tracker.UnregisterCancel(activeID, staleGeneration)

	if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryIgnoredTransition {
		t.Fatalf("gap retry status = %q, want %q", result.Status, activeRequestRetryIgnoredTransition)
	}
	if staleCancelCalls != 0 {
		t.Fatalf("gap retry called stale cancel %d times, want 0", staleCancelCalls)
	}
	if tracker.IsRetryRequested(activeID) {
		t.Fatal("rejected transition retry left a pending retry")
	}
}

func TestActiveRequestTrackerUnregisterReportsPendingRetry(t *testing.T) {
	tracker := newActiveRequestTracker()
	activeID := tracker.Start(&ReqeustLog{UserID: "user-a", Provider: "provider-a"}, time.Now())
	generation := tracker.RegisterCancel(activeID, func() {})
	if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryTriggered {
		t.Fatalf("retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}
	if !tracker.UnregisterCancel(activeID, generation) {
		t.Fatal("unregister did not report a pending retry")
	}
	if result := tracker.Retry(-activeID, "user-a"); result.Status != activeRequestRetryTriggered {
		t.Fatalf("idempotent pending retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}
}

func TestActiveRequestTrackerQueuedStateAndRetryNoop(t *testing.T) {
	tracker := newActiveRequestTracker()
	firstID := tracker.Start(&ReqeustLog{UserID: "user-a", Platform: "openai-chat", Provider: "provider-a"}, time.Now())
	secondID := tracker.Start(&ReqeustLog{UserID: "user-a", Platform: "openai-chat", Provider: "provider-a"}, time.Now())
	queueKey := providerQueueKey("user-a", "openai-chat", "pool-a")

	tracker.MarkQueued(firstID, queueKey, 1)
	tracker.MarkQueued(secondID, queueKey, 2)

	if result := tracker.Retry(-firstID, "user-a"); result.Status != activeRequestRetryIgnoredQueued {
		t.Fatalf("queued retry status = %q, want %q", result.Status, activeRequestRetryIgnoredQueued)
	}

	tracker.UpdateQueuePositions(queueKey, map[int64]int{secondID: 1})
	logs := tracker.List("openai-chat", "", "user-a")
	var foundSecond bool
	for _, logEntry := range logs {
		if logEntry.ID == -secondID {
			foundSecond = true
			if logEntry.Status != requestLogStatusQueued || logEntry.QueuePosition != 1 {
				t.Fatalf("second queued log = status %q position %d, want queued #1", logEntry.Status, logEntry.QueuePosition)
			}
			if logEntry.Provider != "" || logEntry.ErrorMessage != "排队中" {
				t.Fatalf("queued log provider/error = %q/%q, want empty/排队中", logEntry.Provider, logEntry.ErrorMessage)
			}
		}
	}
	if !foundSecond {
		t.Fatal("second queued log not found")
	}
}

func TestActiveRequestTrackerRetryIgnoresFinishedFirstTextAndWrongUser(t *testing.T) {
	tracker := newActiveRequestTracker()

	if result := tracker.Retry(-123, ""); result.Status != activeRequestRetryIgnoredFinished {
		t.Fatalf("missing retry status = %q, want finished", result.Status)
	}

	wrongUserID := tracker.Start(&ReqeustLog{UserID: "user-a"}, time.Now())
	if result := tracker.Retry(-wrongUserID, "user-b"); result.Status != activeRequestRetryIgnoredUnauthorized {
		t.Fatalf("wrong user retry status = %q, want unauthorized", result.Status)
	}

	startedID := tracker.Start(&ReqeustLog{UserID: "user-a"}, time.Now())
	tracker.MarkResponseStarted(startedID)
	tracker.RegisterCancel(startedID, func() {})
	if result := tracker.Retry(-startedID, "user-a"); result.Status != activeRequestRetryIgnoredResponseStarted {
		t.Fatalf("response-started retry status = %q, want %q", result.Status, activeRequestRetryIgnoredResponseStarted)
	}

	firstTextID := tracker.Start(&ReqeustLog{
		UserID:                "user-a",
		FirstTokenDurationSec: 0.42,
		FirstTextSec:          0.42,
	}, time.Now())
	if result := tracker.Retry(-firstTextID, "user-a"); result.Status != activeRequestRetryIgnoredFirstText {
		t.Fatalf("first-text retry status = %q, want %q", result.Status, activeRequestRetryIgnoredFirstText)
	} else if result.FirstTokenDurationSec != 0.42 || result.FirstTextSec != 0.42 {
		t.Fatalf("first-text retry result = %#v, want first token/text seconds", result)
	}
	if tracker.IsRetryRequested(firstTextID) {
		t.Fatalf("first-text request should not be marked retrying")
	}
}

func TestMarkFirstTextSyncsActiveRequest(t *testing.T) {
	previousTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = previousTracker
	})

	now := time.Now().Add(-250 * time.Millisecond)
	requestLog := &ReqeustLog{
		Platform: "openai-responses",
		Provider: "provider-a",
		Model:    "gpt-5",
	}
	requestLog.startedAt = now
	requestLog.ActiveRequestID = defaultActiveRequestTracker.Start(requestLog, now)

	requestLog.markFirstText()

	logs := defaultActiveRequestTracker.List("", "", "")
	if len(logs) != 1 {
		t.Fatalf("active logs count = %d, want 1", len(logs))
	}
	if logs[0].FirstTokenDurationSec <= 0 {
		t.Fatalf("active first_token_duration_sec = %f, want positive", logs[0].FirstTokenDurationSec)
	}
	if logs[0].FirstTextSec <= 0 {
		t.Fatalf("active first_text_sec = %f, want positive", logs[0].FirstTextSec)
	}
}

func TestListActiveRequestLogsForUserReturnsTrackerRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	previousTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = previousTracker
	})

	key, err := NewCodexRelayKeyService().CreateKeyForUser("user-a", "My named relay key")
	if err != nil {
		t.Fatalf("create relay key: %v", err)
	}

	activeID := defaultActiveRequestTracker.Start(&ReqeustLog{
		UserID:     "user-a",
		Platform:   "claude",
		Provider:   "active-provider",
		Model:      "claude-opus",
		IsStream:   true,
		ClientIP:   "127.0.0.2",
		RelayKeyID: key.ID,
	}, time.Now().Add(-time.Second))
	defer defaultActiveRequestTracker.Finish(activeID)

	logs, err := NewLogService().ListActiveRequestLogsForUser("user-a")
	if err != nil {
		t.Fatalf("ListActiveRequestLogsForUser: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs count = %d, want 1", len(logs))
	}
	if logs[0].Status != requestLogStatusProcessing || logs[0].Provider != "active-provider" {
		t.Fatalf("first log = status %q provider %q, want processing active-provider", logs[0].Status, logs[0].Provider)
	}
	if logs[0].ClientIP != "127.0.0.2" {
		t.Fatalf("active client_ip = %q, want 127.0.0.2", logs[0].ClientIP)
	}
	if logs[0].RelayKeyName != "My named relay key" {
		t.Fatalf("active relay key name = %q, want My named relay key", logs[0].RelayKeyName)
	}
}
