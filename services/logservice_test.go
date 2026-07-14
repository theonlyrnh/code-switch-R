package services

import "testing"

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
