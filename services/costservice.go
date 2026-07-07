package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daodao97/xgo/xdb"
)

const costSettingsFileName = "cost-settings.json"

type CostService struct {
	mu sync.Mutex
}

type CostUsageItem struct {
	Platform          string `json:"platform"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	TotalRequests     int64  `json:"total_requests"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheCreateTokens int64  `json:"cache_create_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
}

type CostPrice struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

type CostSettings struct {
	ProviderMultipliers map[string]float64   `json:"provider_multipliers"`
	ModelPriceOverrides map[string]CostPrice `json:"model_price_overrides"`
}

func NewCostService() *CostService {
	return &CostService{}
}

func (cs *CostService) TodayUsage(platform string, provider string) ([]CostUsageItem, error) {
	return cs.TodayUsageForUser("", platform, provider)
}

func (cs *CostService) TodayUsageForUser(userID string, platform string, provider string) ([]CostUsageItem, error) {
	loc := beijingLocation
	start := startOfDay(time.Now().In(loc))
	end := start.Add(24 * time.Hour)
	queryStart := start.In(time.UTC).Format(timeLayout)
	queryEnd := end.In(time.UTC).Format(timeLayout)

	query := `
		SELECT
			COALESCE(platform, '') AS platform,
			COALESCE(provider, '') AS provider,
			COALESCE(model, '') AS model,
			COUNT(*) AS total_requests,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cache_create_tokens), 0) AS cache_create_tokens,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
			COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens
		FROM request_log
		WHERE created_at >= ?
			AND created_at < ?
			AND http_code >= 200
			AND http_code < 300
			AND input_tokens > 0`
	args := []any{queryStart, queryEnd}
	if strings.TrimSpace(platform) != "" {
		query += " AND platform = ?"
		args = append(args, strings.TrimSpace(platform))
	}
	if strings.TrimSpace(provider) != "" {
		query += " AND provider = ?"
		args = append(args, strings.TrimSpace(provider))
	}
	if strings.TrimSpace(userID) != "" {
		query += " AND user_id = ?"
		args = append(args, strings.TrimSpace(userID))
	}
	query += " GROUP BY platform, provider, model ORDER BY platform ASC, provider ASC, model ASC"

	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		if isNoSuchTableErr(err) {
			return []CostUsageItem{}, nil
		}
		return nil, err
	}
	defer rows.Close()

	items := []CostUsageItem{}
	for rows.Next() {
		var item CostUsageItem
		if err := rows.Scan(
			&item.Platform,
			&item.Provider,
			&item.Model,
			&item.TotalRequests,
			&item.InputTokens,
			&item.OutputTokens,
			&item.CacheCreateTokens,
			&item.CacheReadTokens,
			&item.ReasoningTokens,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (cs *CostService) GetSettings() (CostSettings, error) {
	return cs.GetSettingsForUser("")
}

func (cs *CostService) GetSettingsForUser(userID string) (CostSettings, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.loadSettingsLocked(userID)
}

func (cs *CostService) SaveSettings(settings CostSettings) (CostSettings, error) {
	return cs.SaveSettingsForUser("", settings)
}

func (cs *CostService) SaveSettingsForUser(userID string, settings CostSettings) (CostSettings, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	normalized := normalizeCostSettings(settings)
	if err := cs.saveSettingsLocked(userID, normalized); err != nil {
		return normalized, err
	}
	return normalized, nil
}

func (cs *CostService) ResetProviderMultiplier(platform string, provider string) error {
	return cs.ResetProviderMultiplierForUser("", platform, provider)
}

func (cs *CostService) ResetProviderMultiplierForUser(userID string, platform string, provider string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	settings, err := cs.loadSettingsLocked(userID)
	if err != nil {
		return err
	}
	delete(settings.ProviderMultipliers, CostProviderKey(platform, provider))
	return cs.saveSettingsLocked(userID, settings)
}

func (cs *CostService) ResetModelOverride(platform string, provider string, model string) error {
	return cs.ResetModelOverrideForUser("", platform, provider, model)
}

func (cs *CostService) ResetModelOverrideForUser(userID string, platform string, provider string, model string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	settings, err := cs.loadSettingsLocked(userID)
	if err != nil {
		return err
	}
	delete(settings.ModelPriceOverrides, CostModelKey(platform, provider, model))
	return cs.saveSettingsLocked(userID, settings)
}

func (cs *CostService) settingsPath() (string, error) {
	home, err := getUserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, appSettingsDir, costSettingsFileName), nil
}

func (cs *CostService) settingsPathForUser(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		path, err := cs.settingsPath()
		if err != nil {
			return ""
		}
		return path
	}
	dir, err := UserDataDir(userID)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, costSettingsFileName)
}

func (cs *CostService) settingsPathForUserWithError(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return cs.settingsPath()
	}
	dir, err := UserDataDir(userID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, costSettingsFileName), nil
}

func (cs *CostService) loadSettingsLocked(userID string) (CostSettings, error) {
	settings := defaultCostSettings()
	path, err := cs.settingsPathForUserWithError(userID)
	if err != nil {
		return settings, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return settings, err
	}
	if len(data) == 0 {
		return settings, nil
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return defaultCostSettings(), err
	}
	return normalizeCostSettings(settings), nil
}

func (cs *CostService) saveSettingsLocked(userID string, settings CostSettings) error {
	path, err := cs.settingsPathForUserWithError(userID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(normalizeCostSettings(settings), "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o600)
}

func defaultCostSettings() CostSettings {
	return CostSettings{
		ProviderMultipliers: map[string]float64{},
		ModelPriceOverrides: map[string]CostPrice{},
	}
}

func normalizeCostSettings(settings CostSettings) CostSettings {
	result := defaultCostSettings()
	for key, value := range settings.ProviderMultipliers {
		parts := strings.SplitN(key, "::", 2)
		if len(parts) != 2 {
			continue
		}
		multiplier := value
		if multiplier <= 0 {
			multiplier = 1
		}
		result.ProviderMultipliers[CostProviderKey(parts[0], parts[1])] = multiplier
	}
	for key, value := range settings.ModelPriceOverrides {
		parts := strings.SplitN(key, "::", 3)
		if len(parts) != 3 {
			continue
		}
		result.ModelPriceOverrides[CostModelKey(parts[0], parts[1], parts[2])] = normalizeCostPrice(value)
	}
	return result
}

func normalizeCostPrice(price CostPrice) CostPrice {
	if price.Input < 0 {
		price.Input = 0
	}
	if price.Output < 0 {
		price.Output = 0
	}
	if price.CacheRead < 0 {
		price.CacheRead = 0
	}
	return price
}

func CostProviderKey(platform string, provider string) string {
	return strings.TrimSpace(platform) + "::" + strings.TrimSpace(provider)
}

func CostModelKey(platform string, provider string, model string) string {
	return CostProviderKey(platform, provider) + "::" + NormalizeCostModelName(model)
}

func NormalizeCostModelName(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
