package services

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

func setupCostServiceTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := InitDatabase(); err != nil {
		t.Fatalf("InitDatabase failed: %v", err)
	}
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("xdb.DB failed: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM request_log`); err != nil {
		t.Fatalf("clear request_log: %v", err)
	}
}

func insertCostLog(t *testing.T, fields map[string]any) {
	t.Helper()
	record := xdb.Record{
		"platform":            "claude",
		"provider":            "provider-a",
		"model":               "claude-haiku-4-5",
		"http_code":           200,
		"input_tokens":        1000,
		"output_tokens":       2000,
		"cache_create_tokens": 3000,
		"cache_read_tokens":   4000,
		"reasoning_tokens":    5000,
		"created_at":          time.Now().UTC().Format(timeLayout),
	}
	for key, value := range fields {
		record[key] = value
	}
	if _, err := xdb.New("request_log").Insert(record); err != nil {
		t.Fatalf("insert request_log: %v", err)
	}
}

func TestCostServiceTodayUsageGroupsSuccessfulInputTokenRowsByPlatformProviderModel(t *testing.T) {
	setupCostServiceTestDB(t)
	now := time.Now().In(beijingLocation)
	todayUTC := now.Add(-time.Duration(now.Hour()) * time.Hour).In(time.UTC).Format(timeLayout)
	yesterdayUTC := now.AddDate(0, 0, -1).In(time.UTC).Format(timeLayout)

	insertCostLog(t, map[string]any{"created_at": todayUTC, "input_tokens": 100, "output_tokens": 200, "cache_create_tokens": 300, "cache_read_tokens": 400, "reasoning_tokens": 500})
	insertCostLog(t, map[string]any{"created_at": todayUTC, "input_tokens": 7, "output_tokens": 8, "cache_create_tokens": 9, "cache_read_tokens": 10, "reasoning_tokens": 11})
	insertCostLog(t, map[string]any{"created_at": todayUTC, "input_tokens": 13, "output_tokens": 14, "cache_create_tokens": 15, "cache_read_tokens": 16, "reasoning_tokens": 17, "exclude_from_total": 1})
	insertCostLog(t, map[string]any{"created_at": todayUTC, "model": "claude-sonnet-4-5", "input_tokens": 1})
	insertCostLog(t, map[string]any{"created_at": todayUTC, "http_code": 500, "input_tokens": 999})
	insertCostLog(t, map[string]any{"created_at": todayUTC, "input_tokens": 0, "output_tokens": 999})
	insertCostLog(t, map[string]any{"created_at": yesterdayUTC, "input_tokens": 999})
	insertCostLog(t, map[string]any{"created_at": todayUTC, "platform": "openai-chat", "provider": "provider-b", "model": "GPT-5.4", "input_tokens": 3, "output_tokens": 4})

	items, err := NewCostService().TodayUsage("", "")
	if err != nil {
		t.Fatalf("TodayUsage failed: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3: %#v", len(items), items)
	}

	first := items[0]
	if first.Platform != "claude" || first.Provider != "provider-a" || first.Model != "claude-haiku-4-5" {
		t.Fatalf("first group = %#v", first)
	}
	if first.InputTokens != 107 || first.OutputTokens != 208 || first.CacheCreateTokens != 309 || first.CacheReadTokens != 410 || first.ReasoningTokens != 511 || first.TotalRequests != 3 {
		t.Fatalf("first totals = %#v", first)
	}

	filtered, err := NewCostService().TodayUsage("openai-chat", "provider-b")
	if err != nil {
		t.Fatalf("filtered TodayUsage failed: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Model != "GPT-5.4" || filtered[0].InputTokens != 3 {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func TestCostServiceSettingsPersistPerUserAndNormalizeModels(t *testing.T) {
	setupCostServiceTestDB(t)
	service := NewCostService()
	settings := CostSettings{
		ProviderMultipliers: map[string]float64{
			CostProviderKey("openai-chat", "Provider A"): 1.25,
		},
		ModelPriceOverrides: map[string]CostPrice{
			CostModelKey("openai-chat", "Provider A", "GPT-5.4"): {Input: 2, Output: 3, CacheRead: 0.5},
		},
		ModelPrices: map[string]CostPrice{
			" GPT-5.5 ": {Input: 5, Output: 30, CacheRead: 0.5},
		},
	}
	if saved, err := service.SaveSettingsForUser("user-a", settings); err != nil {
		t.Fatalf("SaveSettingsForUser failed: %v", err)
	} else if saved.ProviderMultipliers[CostProviderKey("openai-chat", "Provider A")] != 1.25 {
		t.Fatalf("saved settings = %#v", saved)
	}

	loaded, err := service.GetSettingsForUser("user-a")
	if err != nil {
		t.Fatalf("GetSettingsForUser failed: %v", err)
	}
	if loaded.ModelPriceOverrides[CostModelKey("openai-chat", "Provider A", "gpt-5.4")].Output != 3 {
		t.Fatalf("loaded settings did not normalize model key: %#v", loaded.ModelPriceOverrides)
	}
	if loaded.ModelPrices["gpt-5.5"].Output != 30 {
		t.Fatalf("loaded global model prices did not normalize model key: %#v", loaded.ModelPrices)
	}

	if err := service.ResetProviderMultiplierForUser("user-a", "openai-chat", "Provider A"); err != nil {
		t.Fatalf("ResetProviderMultiplierForUser failed: %v", err)
	}
	if err := service.ResetModelOverrideForUser("user-a", "openai-chat", "Provider A", "GPT-5.4"); err != nil {
		t.Fatalf("ResetModelOverrideForUser failed: %v", err)
	}
	loaded, err = service.GetSettingsForUser("user-a")
	if err != nil {
		t.Fatalf("GetSettingsForUser after reset failed: %v", err)
	}
	if len(loaded.ProviderMultipliers) != 0 || len(loaded.ModelPrices) != 1 || len(loaded.ModelPriceOverrides) != 0 {
		t.Fatalf("reset settings = %#v", loaded)
	}

	userDir, err := UserDataDir("user-a")
	if err != nil {
		t.Fatalf("UserDataDir failed: %v", err)
	}
	if got, want := service.settingsPathForUser("user-a"), filepath.Join(userDir, "cost-settings.json"); got != want {
		t.Fatalf("settings path = %q, want %q", got, want)
	}
}
