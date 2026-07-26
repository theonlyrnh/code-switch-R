package services

import (
	"strings"
	"testing"
	"time"
)

func TestSpecialBlacklistRuleMatchingOrderAndJSON(t *testing.T) {
	pool := &ProviderPool{SpecialBlacklistRules: []SpecialBlacklistRule{
		{ID: "first", Name: "First", HTTPStatus: 429},
		{ID: "second", Name: "Second", HTTPStatus: 429, JSONPath: "error.code", ExpectedJSONValue: `"limit"`},
	}}
	matched := specialBlacklistRuleForFailure(pool, 429, `{"error":{"code":"limit"}}`)
	if matched == nil || matched.ID != "first" {
		t.Fatalf("matched rule = %#v, want first rule", matched)
	}

	pool.SpecialBlacklistRules = pool.SpecialBlacklistRules[1:]
	matched = specialBlacklistRuleForFailure(pool, 429, `{"error":{"code":"limit"}}`)
	if matched == nil || matched.ID != "second" {
		t.Fatalf("matched JSON rule = %#v, want second", matched)
	}
	if matched := specialBlacklistRuleForFailure(pool, 429, `not json`); matched != nil {
		t.Fatalf("non-JSON response matched %#v", matched)
	}
	if matched := specialBlacklistRuleForFailure(pool, 429, `{"error":{"code":429}}`); matched != nil {
		t.Fatalf("different JSON type matched %#v", matched)
	}
}

func TestFirstTextTimeoutMatchesSpecialBlacklistRule(t *testing.T) {
	relay := NewProviderRelayService(NewProviderService(), NewProviderPoolService(), nil, nil, nil, DefaultRelayBindAddr)
	attemptLogs := NewPoolAttemptLogService()
	relay.SetPoolAttemptLogService(attemptLogs)
	rule := SpecialBlacklistRule{
		ID:                "first-text-timeout",
		Name:              "First text timeout",
		HTTPStatus:        504,
		JSONPath:          "error.code",
		ExpectedJSONValue: `"first_text_timeout"`,
		Threshold:         1,
		DurationMinutes:   1,
	}
	pool := &ProviderPool{
		ID:                    "timeout-pool",
		Platform:              "openai-responses",
		Mode:                  ProviderPoolModeManaged,
		AutoBlacklistEnabled:  true,
		SpecialBlacklistRules: []SpecialBlacklistRule{rule},
	}
	provider := Provider{ID: 7, Name: "slow-provider", Enabled: true}

	if matched := specialBlacklistRuleForFailure(pool, 504, firstTextTimeoutErrorBody); matched == nil || matched.ID != rule.ID {
		t.Fatalf("first-text timeout rule match = %#v, want %q", matched, rule.ID)
	}
	if !relay.recordCodexStreamPreflightFailureForUser("user-a", pool.Platform, pool.ID, pool, provider, errCodexFirstTextTimeout) {
		t.Fatal("first-text timeout did not trigger its special blacklist rule")
	}
	entries := attemptLogs.List("user-a", 1, time.Time{})
	if len(entries) != 1 || !strings.Contains(entries[0].Message, firstTextTimeoutErrorBody) {
		t.Fatalf("first-text timeout attempt log = %#v, want JSON body", entries)
	}

	penalty := relay.poolPenalties[penaltyKey("user-a", pool.Platform, pool.ID, provider.ID)]
	if penalty == nil || penalty.RuleFailureCounts[rule.ID] != 1 || penalty.LastReason != rule.Name || time.Until(penalty.BlacklistedUntil) <= 0 {
		t.Fatalf("unexpected first-text timeout penalty: %#v", penalty)
	}
}

func TestSpecialBlacklistRuleUsesIndependentCounterAndSuccessReset(t *testing.T) {
	relay := NewProviderRelayService(NewProviderService(), NewProviderPoolService(), nil, nil, nil, DefaultRelayBindAddr)
	provider := Provider{ID: 1, Name: "provider-a"}
	rule := SpecialBlacklistRule{ID: "rate-limit", Name: "Rate limit", HTTPStatus: 429, Threshold: 2, DurationMinutes: 1}
	pool := &ProviderPool{ID: "pool-a", Platform: "openai-chat", Mode: ProviderPoolModeManaged, AutoBlacklistEnabled: true, AutoBlacklistThreshold: 3, AutoBlacklistDurationMinutes: 10, SpecialBlacklistRules: []SpecialBlacklistRule{rule}}

	if relay.recordProviderFailureWithRuleForUser("user-a", pool.Platform, pool.ID, pool, provider, "HTTP 429", &rule) {
		t.Fatal("first special failure blacklisted provider")
	}
	penalty := relay.poolPenalties[penaltyKey("user-a", pool.Platform, pool.ID, provider.ID)]
	if penalty == nil || penalty.FailureCount != 0 || penalty.RuleFailureCounts[rule.ID] != 1 {
		t.Fatalf("unexpected independent counters: %#v", penalty)
	}
	if !relay.recordProviderFailureWithRuleForUser("user-a", pool.Platform, pool.ID, pool, provider, "HTTP 429", &rule) {
		t.Fatal("second special failure did not blacklist provider")
	}
	if penalty.LastReason != rule.Name || time.Until(penalty.BlacklistedUntil) <= 0 {
		t.Fatalf("unexpected special blacklist state: %#v", penalty)
	}

	relay.recordProviderSuccessForUser("user-a", pool.Platform, pool.ID, provider)
	if _, exists := relay.poolPenalties[penaltyKey("user-a", pool.Platform, pool.ID, provider.ID)]; exists {
		t.Fatal("success did not clear all failure counters")
	}

	relay.recordProviderFailureWithRuleForUser("user-a", pool.Platform, pool.ID, pool, provider, "HTTP 500", nil)
	penalty = relay.poolPenalties[penaltyKey("user-a", pool.Platform, pool.ID, provider.ID)]
	if penalty == nil || penalty.FailureCount != 1 || len(penalty.RuleFailureCounts) != 0 {
		t.Fatalf("global fallback did not use global counter: %#v", penalty)
	}
}

func TestSpecialBlacklistRuleValidation(t *testing.T) {
	pool := &ProviderPool{SpecialBlacklistRules: []SpecialBlacklistRule{{Name: "bad", HTTPStatus: 429, JSONPath: "error.code", ExpectedJSONValue: "not-json", Threshold: 1, DurationMinutes: 1}}}
	if err := normalizeAndValidateSpecialBlacklistRules(pool); err == nil {
		t.Fatal("invalid JSON value was accepted")
	}
	pool.SpecialBlacklistRules = []SpecialBlacklistRule{{Name: "good", HTTPStatus: 429, JSONPath: "error.code", ExpectedJSONValue: `"limit"`, Threshold: 1, DurationMinutes: 1}}
	if err := normalizeAndValidateSpecialBlacklistRules(pool); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	if !strings.HasPrefix(pool.SpecialBlacklistRules[0].ID, "rule_") {
		t.Fatalf("rule id was not generated: %q", pool.SpecialBlacklistRules[0].ID)
	}

	pool.SpecialBlacklistRules[0].DurationMinutes = maxSpecialBlacklistDurationMinutes
	if err := normalizeAndValidateSpecialBlacklistRules(pool); err != nil {
		t.Fatalf("maximum special blacklist duration was rejected: %v", err)
	}
	pool.SpecialBlacklistRules[0].DurationMinutes = maxSpecialBlacklistDurationMinutes + 1
	if err := normalizeAndValidateSpecialBlacklistRules(pool); err == nil {
		t.Fatal("special blacklist duration above maximum was accepted")
	}
}

func TestPoolAttemptLogsAreUserScopedAndRedactAccountKeys(t *testing.T) {
	logs := NewPoolAttemptLogService()
	relay := NewProviderRelayService(NewProviderService(), NewProviderPoolService(), nil, nil, nil, DefaultRelayBindAddr)
	relay.SetPoolAttemptLogService(logs)
	pool := &ProviderPool{ID: "pool-a", Name: "Accounts"}
	provider := Provider{ID: -1, APIKey: "secret-key-9876"}
	rule := &SpecialBlacklistRule{Name: "Rate limit"}
	relay.recordPoolAttemptError("user-a", pool, provider, 429, rule, "upstream rejected secret-key-9876")

	entries := logs.List("user-a", 10, time.Time{})
	if len(entries) != 1 || !strings.Contains(entries[0].Message, "****9876") || !strings.Contains(entries[0].Message, "rule=Rate limit") {
		t.Fatalf("unexpected attempt entry: %#v", entries)
	}
	if strings.Contains(entries[0].Message, provider.APIKey) {
		t.Fatalf("attempt entry leaked account key: %q", entries[0].Message)
	}
	if got := logs.List("user-b", 10, time.Time{}); len(got) != 0 {
		t.Fatalf("other user received attempt logs: %#v", got)
	}
	if got := logs.List("user-a", 10, time.Now()); len(got) != 0 {
		t.Fatalf("clear cutoff did not filter prior logs: %#v", got)
	}
}
