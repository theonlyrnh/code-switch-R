package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

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
