package services

import (
	"strings"
	"testing"
	"time"
)

func TestRelayKeyDisplayName(t *testing.T) {
	names := map[string]string{
		"key-1": "main key",
	}

	if got := relayKeyDisplayName("key-1", names); got != "main key" {
		t.Fatalf("expected key name, got %q", got)
	}
	if got := relayKeyDisplayName("deleted-key", names); got != "deleted-key" {
		t.Fatalf("expected id fallback, got %q", got)
	}
	if got := relayKeyDisplayName("", names); got != "" {
		t.Fatalf("expected empty display name, got %q", got)
	}
}

func TestParseLogTimestampTreatsSQLiteTimestampAsUTCAndReturnsBeijingTime(t *testing.T) {
	createdAt, hasTime := parseLogTimestamp("2026-06-08 07:33:59")
	if !hasTime {
		t.Fatalf("expected timestamp to include time")
	}
	if got := createdAt.Format(timeLayout); got != "2026-06-08 15:33:59" {
		t.Fatalf("createdAt = %q, want Beijing time %q", got, "2026-06-08 15:33:59")
	}
	if got := dayFromTimestamp("2026-06-07 16:00:00"); got != "2026-06-08" {
		t.Fatalf("dayFromTimestamp = %q, want Beijing day %q", got, "2026-06-08")
	}
}

func TestLogServiceTrafficTotalsExcludeMarkedRows(t *testing.T) {
	setupCostServiceTestDB(t)
	insertCostLog(t, map[string]any{
		"input_tokens":  100,
		"output_tokens": 200,
	})
	insertCostLog(t, map[string]any{
		"input_tokens":       300,
		"output_tokens":      400,
		"exclude_from_total": 1,
	})

	service := NewLogService()
	stats, err := service.StatsSince("claude")
	if err != nil {
		t.Fatalf("StatsSince failed: %v", err)
	}
	if stats.TotalRequests != 2 || stats.InputTokens != 100 || stats.OutputTokens != 200 {
		t.Fatalf("stats = %#v, want two requests but only included tokens", stats)
	}

	providers, err := service.ProviderDailyStats("claude")
	if err != nil {
		t.Fatalf("ProviderDailyStats failed: %v", err)
	}
	if len(providers) != 1 || providers[0].TotalRequests != 2 || providers[0].InputTokens != 100 || providers[0].OutputTokens != 200 {
		t.Fatalf("provider stats = %#v, want two requests but only included tokens", providers)
	}
}

func TestLogServiceStatsAggregateByBeijingDayAndUser(t *testing.T) {
	setupCostServiceTestDB(t)
	start := startOfDay(time.Now().In(beijingLocation))
	atHour := func(hour int, minute int) string {
		return start.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute).UTC().Format(timeLayout)
	}

	insertCostLog(t, map[string]any{
		"user_id":             "user-a",
		"platform":            "claude",
		"provider":            " provider-a ",
		"http_code":           201,
		"input_tokens":        10,
		"output_tokens":       20,
		"reasoning_tokens":    30,
		"cache_create_tokens": 40,
		"cache_read_tokens":   50,
		"created_at":          atHour(0, 15),
	})
	insertCostLog(t, map[string]any{
		"user_id":             "user-a",
		"platform":            "claude",
		"provider":            "provider-a",
		"http_code":           500,
		"input_tokens":        100,
		"output_tokens":       200,
		"reasoning_tokens":    300,
		"cache_create_tokens": 400,
		"cache_read_tokens":   500,
		"exclude_from_total":  1,
		"created_at":          atHour(1, 5),
	})
	insertCostLog(t, map[string]any{
		"user_id":             "user-a",
		"platform":            "claude",
		"provider":            "   ",
		"http_code":           0,
		"input_tokens":        1,
		"output_tokens":       2,
		"reasoning_tokens":    3,
		"cache_create_tokens": 4,
		"cache_read_tokens":   5,
		"created_at":          start.Format("2006-01-02"),
	})
	insertCostLog(t, map[string]any{
		"user_id":    "user-b",
		"platform":   "claude",
		"provider":   "provider-b",
		"created_at": atHour(2, 0),
	})
	insertCostLog(t, map[string]any{
		"user_id":    "user-a",
		"platform":   "codex",
		"provider":   "provider-c",
		"created_at": atHour(3, 0),
	})
	insertCostLog(t, map[string]any{
		"user_id":    "user-a",
		"platform":   "claude",
		"provider":   "provider-a",
		"created_at": start.Add(-time.Second).UTC().Format(timeLayout),
	})
	insertCostLog(t, map[string]any{
		"user_id":    "user-a",
		"platform":   "claude",
		"provider":   "provider-a",
		"created_at": start.Add(24 * time.Hour).UTC().Format(timeLayout),
	})

	service := NewLogService()
	stats, err := service.StatsSinceForUser("user-a", "claude")
	if err != nil {
		t.Fatalf("StatsSinceForUser failed: %v", err)
	}
	if stats.TotalRequests != 3 || stats.InputTokens != 11 || stats.OutputTokens != 22 || stats.ReasoningTokens != 33 || stats.CacheCreateTokens != 44 || stats.CacheReadTokens != 55 {
		t.Fatalf("stats = %#v, want user-a claude totals with excluded tokens suppressed", stats)
	}
	if len(stats.Series) != 48 {
		t.Fatalf("len(stats.Series) = %d, want 48", len(stats.Series))
	}
	if stats.Series[0].Day != start.Format(timeLayout) || stats.Series[0].TotalRequests != 2 || stats.Series[0].InputTokens != 11 {
		t.Fatalf("midnight bucket = %#v", stats.Series[0])
	}
	if stats.Series[1].TotalRequests != 0 {
		t.Fatalf("00:30 bucket = %#v, want empty bucket", stats.Series[1])
	}
	if stats.Series[2].Day != start.Add(time.Hour).Format(timeLayout) || stats.Series[2].TotalRequests != 1 || stats.Series[2].InputTokens != 0 {
		t.Fatalf("01:00 bucket = %#v, want excluded request counted without tokens", stats.Series[2])
	}

	providers, err := service.ProviderDailyStatsForUser("user-a", "claude")
	if err != nil {
		t.Fatalf("ProviderDailyStatsForUser failed: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("providers = %#v, want two normalized providers", providers)
	}
	if providers[0].Provider != "provider-a" || providers[0].TotalRequests != 2 || providers[0].SuccessfulRequests != 1 || providers[0].FailedRequests != 1 || providers[0].SuccessRate != 0.5 {
		t.Fatalf("provider-a stats = %#v", providers[0])
	}
	if providers[0].InputTokens != 10 || providers[0].OutputTokens != 20 || providers[0].ReasoningTokens != 30 || providers[0].CacheCreateTokens != 40 || providers[0].CacheReadTokens != 50 {
		t.Fatalf("provider-a token stats = %#v", providers[0])
	}
	if providers[1].Provider != "(unknown)" || providers[1].TotalRequests != 1 || providers[1].SuccessfulRequests != 0 || providers[1].FailedRequests != 1 {
		t.Fatalf("unknown provider stats = %#v", providers[1])
	}
}

func TestListHTTPErrorConsoleLogsForUserFiltersAndOrders(t *testing.T) {
	setupCostServiceTestDB(t)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	insertCostLog(t, map[string]any{
		"user_id":       "user-a",
		"http_code":     404,
		"error_message": "first error",
		"created_at":    base.Format(timeLayout),
	})
	insertCostLog(t, map[string]any{
		"user_id":       "user-a",
		"http_code":     200,
		"error_message": "success",
		"created_at":    base.Add(time.Minute).Format(timeLayout),
	})
	insertCostLog(t, map[string]any{
		"user_id":       "user-a",
		"http_code":     503,
		"error_message": "second error",
		"created_at":    base.Add(2 * time.Minute).Format(timeLayout),
	})
	insertCostLog(t, map[string]any{
		"user_id":       "user-b",
		"http_code":     500,
		"error_message": "other user",
		"created_at":    base.Add(3 * time.Minute).Format(timeLayout),
	})

	service := &LogService{}
	logs, err := service.ListHTTPErrorConsoleLogsForUser(" user-a ", 2, base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("ListHTTPErrorConsoleLogsForUser failed: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("logs = %#v, want two user-a errors", logs)
	}
	if logs[0].Level != "WARN" || !strings.Contains(logs[0].Message, "HTTP 404") || !strings.Contains(logs[0].Message, "first error") {
		t.Fatalf("first log = %#v", logs[0])
	}
	if logs[1].Level != "ERROR" || !strings.Contains(logs[1].Message, "HTTP 503") || !strings.Contains(logs[1].Message, "second error") {
		t.Fatalf("second log = %#v", logs[1])
	}

	recent, err := service.ListHTTPErrorConsoleLogsForUser("user-a", 10, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("recent ListHTTPErrorConsoleLogsForUser failed: %v", err)
	}
	if len(recent) != 1 || !strings.Contains(recent[0].Message, "HTTP 503") {
		t.Fatalf("recent logs = %#v", recent)
	}
}
