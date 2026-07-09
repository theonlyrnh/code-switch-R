package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func setupProviderPoolHTTPTest(t *testing.T, platform string, providers []Provider, pool *ProviderPool) (*ProviderRelayService, *gin.Engine, string, string) {
	t.Helper()

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)
	configDir := filepath.Join(testHome, ".code-switch")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	providerPath, err := providerFilePath(platform)
	if err != nil {
		t.Fatalf("provider path: %v", err)
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	if err := os.WriteFile(providerPath, payload, 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)
	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	if pool.Platform == "" {
		pool.Platform = platform
	}
	poolID, err := poolService.SavePool(pool)
	if err != nil {
		t.Fatalf("save pool: %v", err)
	}

	key, err := keyService.CreateKey("pool-test-key")
	if err != nil {
		t.Fatalf("create relay key: %v", err)
	}
	if err := keyService.SetPoolBinding(key.ID, platform, poolID); err != nil {
		t.Fatalf("bind relay key: %v", err)
	}
	keySecret, err := keyService.GetKeySecret(key.ID)
	if err != nil {
		t.Fatalf("key secret: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)
	return relay, router, keySecret, poolID
}

func serveChatRequest(router *gin.Engine, keySecret string, body string) (*httptest.ResponseRecorder, <-chan struct{}) {
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()
	return w, done
}

func waitForActiveLog(t *testing.T, platform string, predicate func(ReqeustLog) bool) ReqeustLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs := defaultActiveRequestTracker.List(platform, "", "")
		for _, logEntry := range logs {
			if predicate(logEntry) {
				return logEntry
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active log matching predicate was not found")
	return ReqeustLog{}
}

// ========== HTTP 层 fail-closed 集成测试 ==========

// TestHTTPFailClosedNoBinding 有效 key 但无 binding → 403
func TestHTTPFailClosedNoBinding(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: "https://a.example.com", APIKey: "key-a"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	// 创建初始池子，但不给 key 绑定
	_, _ = poolService.EnsureDefaultPool("openai-chat", providers, DefaultPoolSeed{Mode: ProviderPoolModeManaged})

	key, _ := keyService.CreateKey("unbound-key")
	// 故意不绑定 pool

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for key without pool binding, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "未绑定") {
		t.Fatalf("expected '未绑定' in error, got: %s", w.Body.String())
	}
}

// TestHTTPFailClosedBindingToNonexistentPool binding 指向不存在的 pool → 403
func TestHTTPFailClosedBindingToNonexistentPool(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: "https://a.example.com", APIKey: "key-a"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	key, _ := keyService.CreateKey("bad-binding-key")
	// 绑定到一个不存在的 pool
	_ = keyService.SetPoolBinding(key.ID, "openai-chat", "pool_nonexistent_xyz")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for binding to nonexistent pool, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "不存在") {
		t.Fatalf("expected '不存在' in error, got: %s", w.Body.String())
	}
}

// TestHTTPFailClosedPlatformMismatch binding 指向其他 platform 的 pool → 403
func TestHTTPFailClosedPlatformMismatch(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: "https://a.example.com", APIKey: "key-a"},
	}
	for _, platform := range []string{"openai-chat", "openai-responses"} {
		payload, _ := json.Marshal(providerEnvelope{Providers: providers})
		_ = os.WriteFile(filepath.Join(configDir, platform+".json"), payload, 0o600)
	}

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	// 创建 openai-responses 的 pool
	_, _ = poolService.EnsureDefaultPool("openai-responses", providers, DefaultPoolSeed{Mode: ProviderPoolModeManaged})

	key, _ := keyService.CreateKey("mismatch-key")
	// 把 key 的 openai-chat 绑定到 openai-responses 的 pool
	_ = keyService.SetPoolBinding(key.ID, "openai-chat", "pool_openai-responses_default")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for platform mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "不匹配") {
		t.Fatalf("expected '不匹配' in error, got: %s", w.Body.String())
	}
}

// TestHTTPDifferentPoolsSelectDifferentProviders key1/key2 绑定不同 pool，选择不同 provider
func TestHTTPDifferentPoolsSelectDifferentProviders(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("provider-a unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-a","choices":[{"message":{"content":"from-a"}}]}`))
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("provider-b unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a"},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	// Pool A 只有 provider-a
	poolA := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Pool A",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 1, Enabled: true}},
	}
	poolAID, _ := poolService.SavePool(poolA)

	// Pool B 只有 provider-b
	poolB := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Pool B",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 2, Enabled: true}},
	}
	poolBID, _ := poolService.SavePool(poolB)

	key1, _ := keyService.CreateKey("key-pool-a")
	_ = keyService.SetPoolBinding(key1.ID, "openai-chat", poolAID)

	key2, _ := keyService.CreateKey("key-pool-b")
	_ = keyService.SetPoolBinding(key2.ID, "openai-chat", poolBID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	key1Secret, _ := keyService.GetKeySecret(key1.ID)
	req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+key1Secret)
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("key1 expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	if !strings.Contains(w1.Body.String(), `"provider":"provider-a"`) {
		t.Fatalf("key1 should route to provider-a, got: %s", w1.Body.String())
	}

	key2Secret, _ := keyService.GetKeySecret(key2.ID)
	req2 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+key2Secret)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("key2 expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `"provider":"provider-b"`) {
		t.Fatalf("key2 should route to provider-b, got: %s", w2.Body.String())
	}
}

func TestHTTPStickyPrimaryProviderUntilBlacklisted(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providerAHits := 0
	providerBHits := 0

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerAHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"provider-a overloaded"}}`))
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerBHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a"},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	pool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Sticky Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       2,
		AutoBlacklistDurationMinutes: 10,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 1},
		},
	}
	poolID, _ := poolService.SavePool(pool)

	key, _ := keyService.CreateKey("sticky-key")
	_ = keyService.SetPoolBinding(key.ID, "openai-chat", poolID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+keySecret)
		return req
	}

	// 第一次失败：A 仍未拉黑，不能切到同级 B。
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, newRequest())
	if w1.Code != http.StatusBadGateway {
		t.Fatalf("first request expected 502, got %d: %s", w1.Code, w1.Body.String())
	}
	if providerAHits != 1 {
		t.Fatalf("after first request provider-a hits = %d, want 1", providerAHits)
	}
	if providerBHits != 0 {
		t.Fatalf("after first request provider-b hits = %d, want 0", providerBHits)
	}

	// 第二次失败达到阈值：A 被拉黑，当次请求切到 B 并成功。
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, newRequest())
	if w2.Code != http.StatusOK {
		t.Fatalf("second request expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if providerAHits != 2 {
		t.Fatalf("after second request provider-a hits = %d, want 2", providerAHits)
	}
	if providerBHits != 1 {
		t.Fatalf("after second request provider-b hits = %d, want 1", providerBHits)
	}
	if !strings.Contains(w2.Body.String(), `"provider":"provider-b"`) {
		t.Fatalf("second request should route to provider-b after blacklist, got: %s", w2.Body.String())
	}

	// 第三次请求：A 仍在拉黑期，应直接走 B。
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, newRequest())
	if w3.Code != http.StatusOK {
		t.Fatalf("third request expected 200, got %d: %s", w3.Code, w3.Body.String())
	}
	if providerAHits != 2 {
		t.Fatalf("after third request provider-a hits = %d, want still 2", providerAHits)
	}
	if providerBHits != 2 {
		t.Fatalf("after third request provider-b hits = %d, want 2", providerBHits)
	}
	if !strings.Contains(w3.Body.String(), `"provider":"provider-b"`) {
		t.Fatalf("third request should route directly to provider-b, got: %s", w3.Body.String())
	}
}

func TestHTTPModelsSkipsBlacklistedProvider(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providerAHits := 0
	providerBHits := 0

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerAHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"provider-a forbidden"}`))
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerBHits++
		if r.URL.Path != "/v1/models" {
			t.Errorf("expected /v1/models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-b"}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a"},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	pool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Models Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
		},
	}
	poolID, _ := poolService.SavePool(pool)

	key, _ := keyService.CreateKey("models-key")
	_ = keyService.SetPoolBinding(key.ID, "openai-chat", poolID)

	if !relay.recordProviderFailure("openai-chat", poolID, pool, providers[0], "previous upstream 403") {
		t.Fatal("expected provider-a to be blacklisted")
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("models request expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if providerAHits != 0 {
		t.Fatalf("blacklisted provider-a should not be hit, got %d hits", providerAHits)
	}
	if providerBHits != 1 {
		t.Fatalf("provider-b hits = %d, want 1", providerBHits)
	}
	if !strings.Contains(w.Body.String(), "model-b") {
		t.Fatalf("models response should come from provider-b, got: %s", w.Body.String())
	}
}

func TestHTTPProviderConcurrencyManagedFallbackQueueAndCancel(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	aRelease := make(chan struct{})
	bRelease := make(chan struct{})
	var providerAHits int32
	var providerBHits int32
	var providerCHits int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerAHits, 1)
		<-aRelease
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-a","choices":[{"message":{"content":"from-a"}}]}`))
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerBHits, 1)
		<-bRelease
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	upstreamC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCHits, 1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstreamC.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a", MaxConcurrency: 1},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b", MaxConcurrency: 1},
		{ID: 3, Name: "provider-c", Enabled: true, APIURL: upstreamC.URL, APIKey: "key-c", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Concurrency Pool",
		Mode:     ProviderPoolModeManaged,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
			{ProviderID: 3, Enabled: false, Level: 3},
		},
	}
	relay, router, keySecret, poolID := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	w1, done1 := serveChatRequest(router, keySecret, `{"model":"gpt-4","messages":[{"role":"user","content":"first"}]}`)
	for atomic.LoadInt32(&providerAHits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	w2, done2 := serveChatRequest(router, keySecret, `{"model":"gpt-4","messages":[{"role":"user","content":"second"}]}`)
	for atomic.LoadInt32(&providerBHits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	w3, done3 := serveChatRequest(router, keySecret, `{"model":"gpt-4","messages":[{"role":"user","content":"third"}]}`)
	queued := waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})
	if queued.ErrorMessage != "排队中" {
		t.Fatalf("queued error message = %q, want 排队中", queued.ErrorMessage)
	}
	if result := defaultActiveRequestTracker.Retry(queued.ID, ""); result.Status != activeRequestRetryIgnoredQueued {
		t.Fatalf("queued retry status = %q, want %q", result.Status, activeRequestRetryIgnoredQueued)
	}

	ctx, cancelQueued := context.WithCancel(context.Background())
	req4 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"cancel"}]}`)).WithContext(ctx)
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("Authorization", "Bearer "+keySecret)
	w4 := httptest.NewRecorder()
	done4 := make(chan struct{})
	go func() {
		router.ServeHTTP(w4, req4)
		close(done4)
	}()
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 2
	})
	cancelQueued()
	select {
	case <-done4:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled queued request did not return")
	}
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.ID == queued.ID && logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})

	if statuses := relay.ListProviderBlacklistStatus("openai-chat", poolID); len(statuses) != 0 {
		t.Fatalf("concurrency full should not blacklist providers, got %+v", statuses)
	}

	close(aRelease)
	select {
	case <-done3:
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not finish after provider-a released")
	}
	close(bRelease)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("second request did not finish")
	}

	if w1.Code != http.StatusOK || w2.Code != http.StatusOK || w3.Code != http.StatusOK {
		t.Fatalf("codes = first %d second %d third %d, want all 200; third body=%s", w1.Code, w2.Code, w3.Code, w3.Body.String())
	}
	if atomic.LoadInt32(&providerAHits) != 2 {
		t.Fatalf("provider-a hits = %d, want 2", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 1 {
		t.Fatalf("provider-b hits = %d, want 1", providerBHits)
	}
	if atomic.LoadInt32(&providerCHits) != 0 {
		t.Fatalf("disabled provider-c should not be hit, got %d hits", providerCHits)
	}
}

func TestHTTPProviderConcurrencyQueuesAfterFailedProviderWhenRemainingProviderFull(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)
	configDir := filepath.Join(testHome, ".code-switch")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	var providerAHits int32
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerAHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"provider-a forbidden"}`))
	}))
	defer upstreamA.Close()

	bRelease := make(chan struct{})
	var providerBHits int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := atomic.AddInt32(&providerBHits, 1)
		if hit == 1 {
			<-bRelease
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a", MaxConcurrency: 1},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b", MaxConcurrency: 1},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	if err := os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	relay := NewProviderRelayService(providerService, poolService, keyService, NewNotificationService(appSettings), appSettings, DefaultRelayBindAddr)

	blockerPoolID, err := poolService.SavePool(&ProviderPool{
		Platform: "openai-chat",
		Name:     "Blocker Pool",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 2, Enabled: true, Level: 1}},
	})
	if err != nil {
		t.Fatalf("save blocker pool: %v", err)
	}
	targetPool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Fallback Queue Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
		},
	}
	targetPoolID, err := poolService.SavePool(targetPool)
	if err != nil {
		t.Fatalf("save target pool: %v", err)
	}

	blockerKey, err := keyService.CreateKey("blocker-key")
	if err != nil {
		t.Fatalf("create blocker key: %v", err)
	}
	if err := keyService.SetPoolBinding(blockerKey.ID, "openai-chat", blockerPoolID); err != nil {
		t.Fatalf("bind blocker key: %v", err)
	}
	targetKey, err := keyService.CreateKey("target-key")
	if err != nil {
		t.Fatalf("create target key: %v", err)
	}
	if err := keyService.SetPoolBinding(targetKey.ID, "openai-chat", targetPoolID); err != nil {
		t.Fatalf("bind target key: %v", err)
	}
	blockerSecret, _ := keyService.GetKeySecret(blockerKey.ID)
	targetSecret, _ := keyService.GetKeySecret(targetKey.ID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	wBlocker, doneBlocker := serveChatRequest(router, blockerSecret, `{"model":"gpt-4","messages":[{"role":"user","content":"block b"}]}`)
	for atomic.LoadInt32(&providerBHits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	wTarget, doneTarget := serveChatRequest(router, targetSecret, `{"model":"gpt-4","messages":[{"role":"user","content":"queue after a fails"}]}`)
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})

	if atomic.LoadInt32(&providerAHits) != 1 {
		t.Fatalf("provider-a hits = %d, want 1", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 1 {
		t.Fatalf("target request should queue before hitting full provider-b, hits=%d", providerBHits)
	}

	close(bRelease)
	select {
	case <-doneBlocker:
	case <-time.After(2 * time.Second):
		t.Fatal("blocker request did not finish")
	}
	select {
	case <-doneTarget:
	case <-time.After(2 * time.Second):
		t.Fatal("target request did not finish after provider-b released")
	}

	if wBlocker.Code != http.StatusOK || wTarget.Code != http.StatusOK {
		t.Fatalf("codes = blocker %d target %d, want both 200; target body=%s", wBlocker.Code, wTarget.Code, wTarget.Body.String())
	}
	if atomic.LoadInt32(&providerBHits) != 2 {
		t.Fatalf("provider-b hits = %d, want 2", providerBHits)
	}
	if statuses := relay.ListProviderBlacklistStatus("openai-chat", targetPoolID); len(statuses) != 1 {
		t.Fatalf("provider-a should be blacklisted in target pool, got %+v", statuses)
	}
}

func TestHTTPProviderConcurrencyClearsStaleReservationAfterProviderBlacklisted(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)
	configDir := filepath.Join(testHome, ".code-switch")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	aRelease := make(chan struct{})
	bRelease := make(chan struct{})
	var closeA sync.Once
	var closeB sync.Once
	t.Cleanup(func() {
		closeA.Do(func() { close(aRelease) })
		closeB.Do(func() { close(bRelease) })
	})

	var providerAHits int32
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := atomic.AddInt32(&providerAHits, 1)
		if hit == 1 {
			<-aRelease
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"provider-a forbidden"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"provider-a should stay blacklisted"}`))
	}))
	defer upstreamA.Close()

	var providerBHits int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := atomic.AddInt32(&providerBHits, 1)
		if hit == 1 {
			<-bRelease
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a", MaxConcurrency: 1},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b", MaxConcurrency: 1},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	if err := os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	relay := NewProviderRelayService(providerService, poolService, keyService, NewNotificationService(appSettings), appSettings, DefaultRelayBindAddr)

	blockerPoolID, err := poolService.SavePool(&ProviderPool{
		Platform: "openai-chat",
		Name:     "Stale Reservation Blocker Pool",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 2, Enabled: true, Level: 1}},
	})
	if err != nil {
		t.Fatalf("save blocker pool: %v", err)
	}
	targetPoolID, err := poolService.SavePool(&ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Stale Reservation Target Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
		},
	})
	if err != nil {
		t.Fatalf("save target pool: %v", err)
	}

	blockerKey, _ := keyService.CreateKey("stale-reservation-blocker")
	targetKey, _ := keyService.CreateKey("stale-reservation-target")
	_ = keyService.SetPoolBinding(blockerKey.ID, "openai-chat", blockerPoolID)
	_ = keyService.SetPoolBinding(targetKey.ID, "openai-chat", targetPoolID)
	blockerSecret, _ := keyService.GetKeySecret(blockerKey.ID)
	targetSecret, _ := keyService.GetKeySecret(targetKey.ID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	wBlocker, doneBlocker := serveChatRequest(router, blockerSecret, `{"model":"gpt-4","messages":[{"role":"user","content":"block b"}]}`)
	for atomic.LoadInt32(&providerBHits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	wFirst, doneFirst := serveChatRequest(router, targetSecret, `{"model":"gpt-4","messages":[{"role":"user","content":"first hits a"}]}`)
	for atomic.LoadInt32(&providerAHits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	wQueued, doneQueued := serveChatRequest(router, targetSecret, `{"model":"gpt-4","messages":[{"role":"user","content":"queued should use b"}]}`)
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})

	closeA.Do(func() { close(aRelease) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if statuses := relay.ListProviderBlacklistStatus("openai-chat", targetPoolID); len(statuses) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if statuses := relay.ListProviderBlacklistStatus("openai-chat", targetPoolID); len(statuses) != 1 {
		t.Fatalf("provider-a should be blacklisted before provider-b releases, got %+v", statuses)
	}

	closeB.Do(func() { close(bRelease) })
	for name, done := range map[string]<-chan struct{}{
		"blocker": doneBlocker,
		"first":   doneFirst,
		"queued":  doneQueued,
	} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s request did not finish", name)
		}
	}

	if wBlocker.Code != http.StatusOK || wFirst.Code != http.StatusOK || wQueued.Code != http.StatusOK {
		t.Fatalf("codes = blocker %d first %d queued %d, want all 200; queued body=%s", wBlocker.Code, wFirst.Code, wQueued.Code, wQueued.Body.String())
	}
	if atomic.LoadInt32(&providerAHits) != 1 {
		t.Fatalf("provider-a hits = %d, want 1", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 3 {
		t.Fatalf("provider-b hits = %d, want 3", providerBHits)
	}
	if !strings.Contains(wQueued.Body.String(), `"provider":"provider-b"`) {
		t.Fatalf("queued request should route to provider-b after stale reservation clears, got: %s", wQueued.Body.String())
	}
}

func TestHTTPProviderConcurrencyManualModeQueuesWithoutFallback(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	aRelease := make(chan struct{})
	var providerAHits int32
	var providerBHits int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerAHits, 1)
		<-aRelease
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-a","choices":[{"message":{"content":"from-a"}}]}`))
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerBHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	manualProviderID := int64(1)
	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a", MaxConcurrency: 1},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform:         "openai-chat",
		Name:             "Manual Concurrency Pool",
		Mode:             ProviderPoolModeManual,
		ManualProviderID: &manualProviderID,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true},
			{ProviderID: 2, Enabled: true},
		},
	}
	_, router, keySecret, _ := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	w1, done1 := serveChatRequest(router, keySecret, `{"model":"gpt-4","messages":[{"role":"user","content":"first"}]}`)
	for atomic.LoadInt32(&providerAHits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	w2, done2 := serveChatRequest(router, keySecret, `{"model":"gpt-4","messages":[{"role":"user","content":"second"}]}`)
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})
	if atomic.LoadInt32(&providerBHits) != 0 {
		t.Fatalf("manual mode should not fallback to provider-b, hits=%d", providerBHits)
	}

	close(aRelease)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first manual request did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("queued manual request did not finish")
	}
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("manual codes = first %d second %d, want 200", w1.Code, w2.Code)
	}
	if atomic.LoadInt32(&providerAHits) != 2 {
		t.Fatalf("provider-a hits = %d, want 2", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 0 {
		t.Fatalf("provider-b hits = %d, want 0", providerBHits)
	}
}

func TestHTTPProviderConcurrencyStreamingRequestHoldsSlotUntilFinished(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	streamRelease := make(chan struct{})
	streamStarted := make(chan struct{}, 2)
	var hits int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		streamStarted <- struct{}{}
		<-streamRelease
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstream.URL, APIKey: "key-a", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Streaming Concurrency Pool",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	_, router, keySecret, _ := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	body := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"stream"}]}`
	w1, done1 := serveChatRequest(router, keySecret, body)
	select {
	case <-streamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream did not start")
	}

	w2, done2 := serveChatRequest(router, keySecret, body)
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("second stream should be queued before first stream finishes, hits=%d", hits)
	}

	close(streamRelease)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("queued stream did not finish")
	}
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("stream codes = first %d second %d, want both 200", w1.Code, w2.Code)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("upstream stream hits = %d, want 2", hits)
	}
}

func TestHTTPProviderConcurrencySharedProviderWakesQueuedDifferentPool(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)
	configDir := filepath.Join(testHome, ".code-switch")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	release := make(chan struct{})
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-a","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstream.URL, APIKey: "key-a", MaxConcurrency: 1},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	if err := os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	relay := NewProviderRelayService(providerService, poolService, keyService, NewNotificationService(appSettings), appSettings, DefaultRelayBindAddr)

	poolA := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Pool A",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	poolAID, err := poolService.SavePool(poolA)
	if err != nil {
		t.Fatalf("save pool A: %v", err)
	}
	poolB := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Pool B",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	poolBID, err := poolService.SavePool(poolB)
	if err != nil {
		t.Fatalf("save pool B: %v", err)
	}
	keyA, _ := keyService.CreateKey("key-a")
	keyB, _ := keyService.CreateKey("key-b")
	_ = keyService.SetPoolBinding(keyA.ID, "openai-chat", poolAID)
	_ = keyService.SetPoolBinding(keyB.ID, "openai-chat", poolBID)
	secretA, _ := keyService.GetKeySecret(keyA.ID)
	secretB, _ := keyService.GetKeySecret(keyB.ID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	w1, done1 := serveChatRequest(router, secretA, `{"model":"gpt-4","messages":[{"role":"user","content":"pool-a"}]}`)
	for atomic.LoadInt32(&hits) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	w2, done2 := serveChatRequest(router, secretB, `{"model":"gpt-4","messages":[{"role":"user","content":"pool-b"}]}`)
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("pool B request should be queued while shared provider is occupied, hits=%d", hits)
	}

	close(release)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("pool A request did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("pool B queued request did not wake after shared provider release")
	}
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("shared provider codes = poolA %d poolB %d, want both 200", w1.Code, w2.Code)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("shared provider hits = %d, want 2", hits)
	}
}

func TestHTTPClientCancelBeforeUpstreamHeadersDoesNotBlacklistProvider(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-unblock
	}))
	defer upstream.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstream.URL, APIKey: "key-a", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Cancel Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members:                      []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	relay, router, keySecret, poolID := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"cancel"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(unblock)
		t.Fatal("cancelled in-flight request did not return")
	}
	close(unblock)
	if statuses := relay.ListProviderBlacklistStatus("openai-chat", poolID); len(statuses) != 0 {
		t.Fatalf("client cancellation should not blacklist provider, got %+v", statuses)
	}
}

func TestHTTPClientCancelDuringNonStreamingBodyDoesNotBlacklistProvider(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	headersSent := make(chan struct{}, 1)
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		headersSent <- struct{}{}
		<-unblock
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstream.URL, APIKey: "key-a", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Cancel Body Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members:                      []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	relay, router, keySecret, poolID := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"cancel body"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-headersSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream headers were not sent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(unblock)
		t.Fatal("cancelled body read did not return")
	}
	close(unblock)
	if statuses := relay.ListProviderBlacklistStatus("openai-chat", poolID); len(statuses) != 0 {
		t.Fatalf("client cancellation during body read should not blacklist provider, got %+v", statuses)
	}
}

func TestHTTPModelsClientCancelDoesNotBlacklistProvider(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-unblock
	}))
	defer upstream.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstream.URL, APIKey: "key-a", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Models Cancel Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members:                      []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	relay, router, keySecret, poolID := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+keySecret)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream models request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(unblock)
		t.Fatal("cancelled models request did not return")
	}
	close(unblock)
	if statuses := relay.ListProviderBlacklistStatus("openai-chat", poolID); len(statuses) != 0 {
		t.Fatalf("models client cancellation should not blacklist provider, got %+v", statuses)
	}
}

func TestHTTPProcessingRetryEnqueuesBehindExistingQueue(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	firstEntered := make(chan struct{}, 1)
	order := make(chan string, 2)
	var hits int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hit := atomic.AddInt32(&hits, 1)
		if hit == 1 {
			firstEntered <- struct{}{}
			<-r.Context().Done()
			return
		}
		order <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-a","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstream.URL, APIKey: "key-a", MaxConcurrency: 1},
	}
	pool := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Retry FIFO Pool",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	_, router, keySecret, _ := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	firstBody := `{"model":"gpt-4","messages":[{"role":"user","content":"first"}]}`
	secondBody := `{"model":"gpt-4","messages":[{"role":"user","content":"second"}]}`
	w1, done1 := serveChatRequest(router, keySecret, firstBody)
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first upstream request did not start")
	}

	w2, done2 := serveChatRequest(router, keySecret, secondBody)
	waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusQueued && logEntry.QueuePosition == 1
	})
	processing := waitForActiveLog(t, "openai-chat", func(logEntry ReqeustLog) bool {
		return logEntry.Status == requestLogStatusProcessing && logEntry.Provider == "provider-a"
	})
	if result := defaultActiveRequestTracker.Retry(processing.ID, ""); result.Status != activeRequestRetryTriggered {
		t.Fatalf("processing retry status = %q, want %q", result.Status, activeRequestRetryTriggered)
	}

	var firstAfterRetry string
	var secondAfterRetry string
	select {
	case firstAfterRetry = <-order:
	case <-time.After(2 * time.Second):
		t.Fatal("first queued turn did not reach upstream")
	}
	select {
	case secondAfterRetry = <-order:
	case <-time.After(2 * time.Second):
		t.Fatal("retried request did not reach upstream after queued request")
	}

	if !strings.Contains(firstAfterRetry, "second") {
		t.Fatalf("first request after retry should be existing queued request, got %s", firstAfterRetry)
	}
	if !strings.Contains(secondAfterRetry, "first") {
		t.Fatalf("retried request should run after existing queue, got %s", secondAfterRetry)
	}

	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("retried first request did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("queued second request did not finish")
	}
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("retry FIFO codes = first %d second %d, want both 200", w1.Code, w2.Code)
	}
}

func TestHTTPRetryRereadsCurrentProviderPriority(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providerAEntered := make(chan struct{}, 1)
	var providerAHits int32
	var providerBHits int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerAHits, 1)
		providerAEntered <- struct{}{}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"error":"provider-a was not retried"}`))
		}
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerBHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a"},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	pool := &ProviderPool{
		Platform: "openai-chat",
		Name:     "Retry Pool",
		Mode:     ProviderPoolModeManaged,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
		},
	}
	poolID, _ := poolService.SavePool(pool)

	key, _ := keyService.CreateKey("retry-key")
	_ = keyService.SetPoolBinding(key.ID, "openai-chat", poolID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-providerAEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider-a was not called")
	}

	pool.Members = []ProviderPoolMember{
		{ProviderID: 2, Enabled: true, Level: 1},
		{ProviderID: 1, Enabled: true, Level: 2},
	}
	if _, err := poolService.SavePool(pool); err != nil {
		t.Fatalf("update pool priority: %v", err)
	}

	var retryResult ActiveRequestRetryResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active := defaultActiveRequestTracker.List("openai-chat", "", "")
		if len(active) > 0 {
			retryResult = defaultActiveRequestTracker.Retry(active[0].ID, "")
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retryResult.Status != activeRequestRetryTriggered {
		t.Fatalf("retry status = %q, want %q", retryResult.Status, activeRequestRetryTriggered)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish after retry")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("retry request expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&providerAHits) != 1 {
		t.Fatalf("provider-a hits = %d, want 1", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 1 {
		t.Fatalf("provider-b hits = %d, want 1", providerBHits)
	}
	if !strings.Contains(w.Body.String(), `"provider":"provider-b"`) {
		t.Fatalf("retry should route to current highest priority provider-b, got: %s", w.Body.String())
	}
}

func TestHTTPRetryRereadsCurrentManualProvider(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providerAEntered := make(chan struct{}, 1)
	var providerAHits int32
	var providerBHits int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerAHits, 1)
		providerAEntered <- struct{}{}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"error":"provider-a was not retried"}`))
		}
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerBHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"provider-b","choices":[{"message":{"content":"from-b"}}]}`))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a"},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-chat.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	manualA := int64(1)
	manualB := int64(2)
	pool := &ProviderPool{
		Platform:         "openai-chat",
		Name:             "Manual Retry Pool",
		Mode:             ProviderPoolModeManual,
		ManualProviderID: &manualA,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true},
			{ProviderID: 2, Enabled: false},
		},
	}
	poolID, _ := poolService.SavePool(pool)

	key, _ := keyService.CreateKey("manual-retry-key")
	_ = keyService.SetPoolBinding(key.ID, "openai-chat", poolID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-providerAEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider-a was not called")
	}

	pool.ManualProviderID = &manualB
	if _, err := poolService.SavePool(pool); err != nil {
		t.Fatalf("update manual provider: %v", err)
	}

	var retryResult ActiveRequestRetryResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active := defaultActiveRequestTracker.List("openai-chat", "", "")
		if len(active) > 0 {
			retryResult = defaultActiveRequestTracker.Retry(active[0].ID, "")
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retryResult.Status != activeRequestRetryTriggered {
		t.Fatalf("retry status = %q, want %q", retryResult.Status, activeRequestRetryTriggered)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish after retry")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("manual retry request expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&providerAHits) != 1 {
		t.Fatalf("provider-a hits = %d, want 1", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 1 {
		t.Fatalf("provider-b hits = %d, want 1", providerBHits)
	}
	if !strings.Contains(w.Body.String(), `"provider":"provider-b"`) {
		t.Fatalf("retry should route to current manual provider-b, got: %s", w.Body.String())
	}
}

func TestHTTPRetryDuringCodexEmptyStreamRetryRereadsProviderPriority(t *testing.T) {
	oldTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = oldTracker
	})

	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	configDir := filepath.Join(testHome, ".code-switch")
	_ = os.MkdirAll(configDir, 0o700)

	providerASecondAttempt := make(chan struct{}, 1)
	var providerAHits int32
	var providerBHits int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := atomic.AddInt32(&providerAHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if hit == 1 {
			return
		}
		providerASecondAttempt <- struct{}{}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte("data: {\"error\":\"provider-a was not retried\"}\n\n"))
		}
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerBHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"from-b\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamB.Close()

	providers := []Provider{
		{ID: 1, Name: "provider-a", Enabled: true, APIURL: upstreamA.URL, APIKey: "key-a"},
		{ID: 2, Name: "provider-b", Enabled: true, APIURL: upstreamB.URL, APIKey: "key-b"},
	}
	payload, _ := json.Marshal(providerEnvelope{Providers: providers})
	_ = os.WriteFile(filepath.Join(configDir, "openai-responses.json"), payload, 0o600)

	providerService := NewProviderService()
	poolService := NewProviderPoolService()
	keyService := NewCodexRelayKeyService()
	poolService.SetBindingChecker(keyService)
	appSettings := NewAppSettingsService(nil)
	notificationService := NewNotificationService(appSettings)

	relay := NewProviderRelayService(
		providerService, poolService, keyService,
		notificationService, appSettings,
		DefaultRelayBindAddr,
	)

	pool := &ProviderPool{
		Platform:                     "openai-responses",
		Name:                         "Responses Retry Pool",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       2,
		AutoBlacklistDurationMinutes: 10,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
		},
	}
	poolID, _ := poolService.SavePool(pool)

	key, _ := keyService.CreateKey("responses-retry-key")
	_ = keyService.SetPoolBinding(key.ID, "openai-responses", poolID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	relay.registerRoutes(router)

	keySecret, _ := keyService.GetKeySecret(key.ID)
	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keySecret)

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-providerASecondAttempt:
	case <-time.After(4 * time.Second):
		t.Fatal("provider-a second empty-stream retry attempt was not called")
	}

	pool.Members = []ProviderPoolMember{
		{ProviderID: 2, Enabled: true, Level: 1},
		{ProviderID: 1, Enabled: true, Level: 2},
	}
	if _, err := poolService.SavePool(pool); err != nil {
		t.Fatalf("update responses pool priority: %v", err)
	}

	var retryResult ActiveRequestRetryResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active := defaultActiveRequestTracker.List("openai-responses", "", "")
		if len(active) > 0 {
			retryResult = defaultActiveRequestTracker.Retry(active[0].ID, "")
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retryResult.Status != activeRequestRetryTriggered {
		t.Fatalf("retry status = %q, want %q", retryResult.Status, activeRequestRetryTriggered)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("responses request did not finish after retry")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("responses retry request expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&providerAHits) != 2 {
		t.Fatalf("provider-a hits = %d, want 2", providerAHits)
	}
	if atomic.LoadInt32(&providerBHits) != 1 {
		t.Fatalf("provider-b hits = %d, want 1", providerBHits)
	}
	if !strings.Contains(w.Body.String(), "from-b") {
		t.Fatalf("retry should route to current highest priority provider-b, got: %s", w.Body.String())
	}
	blacklisted := relay.ListProviderBlacklistStatus("openai-responses", poolID)
	if len(blacklisted) != 0 {
		t.Fatalf("user-triggered retry should not blacklist provider-a, got: %+v", blacklisted)
	}
}
