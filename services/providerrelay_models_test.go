package services

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	testRelayEnvOnce sync.Once
	testRelayEnvErr  error
)

func setupRelayTestEnv(t *testing.T) {
	t.Helper()

	testRelayEnvOnce.Do(func() {
		testHome, err := os.MkdirTemp("", "codeswitch-services-test-*")
		if err != nil {
			testRelayEnvErr = err
			return
		}

		if err := os.Setenv("HOME", testHome); err != nil {
			testRelayEnvErr = err
			return
		}

		testRelayEnvErr = InitDatabase()
	})

	if testRelayEnvErr != nil {
		t.Fatalf("初始化测试环境失败: %v", testRelayEnvErr)
	}
}

func newTestRelayService(t *testing.T) (*ProviderService, *ProviderRelayService) {
	t.Helper()
	setupRelayTestEnv(t)

	homeDir, err := getUserHomeDir()
	if err != nil {
		t.Fatalf("获取测试 home 目录失败: %v", err)
	}
	_ = os.Remove(filepath.Join(homeDir, ".code-switch", "claude-code.json"))
	_ = os.Remove(filepath.Join(homeDir, ".code-switch", "codex.json"))
	_ = os.Remove(filepath.Join(homeDir, ".code-switch", providerPoolsFile))
	_ = os.Remove(filepath.Join(homeDir, ".code-switch", codexRelayKeysFile))
	_ = os.RemoveAll(filepath.Join(homeDir, ".code-switch", "providers"))
	_ = os.Remove(filepath.Join(homeDir, ".codex", "config.toml"))
	_ = os.Remove(filepath.Join(homeDir, ".codex", "auth.json"))

	providerService := NewProviderService()
	appSettings := NewAppSettingsService(nil)
	codexRelayKeys := NewCodexRelayKeyService()
	notificationService := NewNotificationService(appSettings)

	relayService := NewProviderRelayService(
		providerService,
		nil, // poolService
		codexRelayKeys,
		notificationService,
		appSettings,
		"",
	)

	// 为测试创建默认池子和 relay key 绑定
	if err := relayService.EnsureDefaultPoolsAndBindings(); err != nil {
		t.Logf("[WARN] 测试环境初始化默认池子失败: %v", err)
	}

	return providerService, relayService
}

// TestModelsHandler 测试 /v1/models 端点处理器
func TestModelsHandler(t *testing.T) {
	// 设置测试环境
	gin.SetMode(gin.TestMode)

	// 创建模拟的上游服务器
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求方法
		if r.Method != "GET" {
			t.Errorf("期望 GET 请求，收到 %s", r.Method)
		}

		// 验证路径
		if r.URL.Path != "/v1/models" {
			t.Errorf("期望路径 /v1/models，收到 %s", r.URL.Path)
		}

		// 验证 Authorization 头
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			t.Error("缺少 Authorization 头")
		}
		if authHeader != "Bearer test-api-key" {
			t.Errorf("Authorization 头不正确，期望 'Bearer test-api-key'，收到 '%s'", authHeader)
		}

		// 返回模拟的模型列表
		response := map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{
					"id":       "claude-sonnet-4",
					"object":   "model",
					"created":  1234567890,
					"owned_by": "anthropic",
				},
				{
					"id":       "claude-opus-4",
					"object":   "model",
					"created":  1234567890,
					"owned_by": "anthropic",
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer upstreamServer.Close()

	// 创建测试用的 ProviderService
	providerService, relayService := newTestRelayService(t)

	// 创建测试用的 provider（使用模拟服务器的 URL）
	testProvider := Provider{
		ID:             1,
		Name:           "TestProvider",
		APIURL:         upstreamServer.URL,
		APIKey:         "test-api-key",
		Enabled:        true,
		Level:          1,
		MaxConcurrency: defaultProviderMaxConcurrency,
	}

	// 保存 provider 配置
	err := providerService.SaveProviders("claude", []Provider{testProvider})
	if err != nil {
		t.Fatalf("保存 provider 配置失败: %v", err)
	}

	// 创建测试路由
	router := gin.New()
	relayService.registerRoutes(router)

	relayKey, err := relayService.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("创建 relay key 失败: %v", err)
	}
	pool := &ProviderPool{
		Platform: "claude",
		Name:     "Models Pool",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{{ProviderID: 1, Enabled: true, Level: 1}},
	}
	poolID, err := relayService.poolService.SavePool(pool)
	if err != nil {
		t.Fatalf("创建模型列表测试池失败: %v", err)
	}
	if err := relayService.codexRelayKeys.SetPoolBinding(relayKey.ID, "claude", poolID); err != nil {
		t.Fatalf("绑定模型列表测试池失败: %v", err)
	}

	// 创建测试请求
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+relayKey.Key)
	w := httptest.NewRecorder()

	// 执行请求
	router.ServeHTTP(w, req)

	// 验证响应状态码
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 %d，收到 %d", http.StatusOK, w.Code)
		t.Logf("响应体: %s", w.Body.String())
	}

	// 验证响应内容类型
	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("期望 Content-Type 为 'application/json'，收到 '%s'", contentType)
	}

	// 验证响应体可以解析为 JSON
	var response map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &response)
	if err != nil {
		t.Errorf("响应体不是有效的 JSON: %v", err)
		t.Logf("响应体: %s", w.Body.String())
	}

	// 验证响应包含 data 字段
	if _, ok := response["data"]; !ok {
		t.Error("响应缺少 'data' 字段")
	}
}

func TestModelsHandlerManagedPoolMergesMapsAndDeduplicates(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	newSuccessfulUpstream := func(name string, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- name
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}

	upstreamA := newSuccessfulUpstream("a", `{"object":"list","data":[{"id":"vendor/shared","object":"model","owned_by":"a"},{"id":"a-only","object":"model","owned_by":"a"}]}`)
	defer upstreamA.Close()
	upstreamB := newSuccessfulUpstream("b", `{"object":"list","data":[{"id":"public-shared","object":"model","owned_by":"b"},{"id":"b-only","object":"model","owned_by":"b"}]}`)
	defer upstreamB.Close()
	upstreamFailure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	}))
	defer upstreamFailure.Close()

	providers := []Provider{
		{
			ID:              1,
			Name:            "provider-a",
			APIURL:          upstreamA.URL,
			APIKey:          "key-a",
			Enabled:         true,
			SupportedModels: map[string]bool{"vendor/shared": true, "a-only": true},
			ModelMapping:    map[string]string{"public-shared": "vendor/shared"},
		},
		{ID: 2, Name: "provider-b", APIURL: upstreamB.URL, APIKey: "key-b", Enabled: true},
		{ID: 3, Name: "provider-failure", APIURL: upstreamFailure.URL, APIKey: "key-c", Enabled: true},
	}
	pool := &ProviderPool{
		Platform:                     "openai-chat",
		Name:                         "Managed Models Merge",
		Mode:                         ProviderPoolModeManaged,
		AutoBlacklistEnabled:         true,
		AutoBlacklistThreshold:       1,
		AutoBlacklistDurationMinutes: 10,
		Members: []ProviderPoolMember{
			{ProviderID: 1, Enabled: true, Level: 1},
			{ProviderID: 2, Enabled: true, Level: 2},
			{ProviderID: 3, Enabled: true, Level: 3},
		},
	}
	relay, router, relayKey, poolID := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	req.Header.Set("Authorization", "Bearer "+relayKey)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	seenStarts := map[string]bool{}
	for len(seenStarts) < 2 {
		select {
		case name := <-started:
			seenStarts[name] = true
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("managed models providers were not queried concurrently")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("managed models request did not complete")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("managed models status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode managed models response: %v", err)
	}
	if response.Object != "list" {
		t.Fatalf("managed models object = %q, want list", response.Object)
	}
	gotIDs := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		gotIDs = append(gotIDs, model.ID)
	}
	if got, want := strings.Join(gotIDs, ","), "public-shared,a-only,b-only"; got != want {
		t.Fatalf("managed model IDs = %q, want %q", got, want)
	}
	if response.Data[0].OwnedBy != "a" {
		t.Fatalf("duplicate model should preserve first provider object, got owned_by=%q", response.Data[0].OwnedBy)
	}
	if strings.Contains(w.Body.String(), "vendor/shared") {
		t.Fatalf("mapped internal model leaked into response: %s", w.Body.String())
	}
	if statuses := relay.ListProviderBlacklistStatus("openai-chat", poolID); len(statuses) != 1 || statuses[0].ProviderID != 3 {
		t.Fatalf("failed provider blacklist status = %+v, want provider 3", statuses)
	}
}

func TestModelsHandlerManualPoolAppliesWildcardMapping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"vendor/model-one","object":"model","owned_by":"vendor"},{"id":"native","object":"model"}]}`))
	}))
	defer upstream.Close()

	providerID := int64(1)
	providers := []Provider{{
		ID:           providerID,
		Name:         "manual-provider",
		APIURL:       upstream.URL,
		APIKey:       "provider-key",
		Enabled:      true,
		ModelMapping: map[string]string{"public/*": "vendor/*"},
	}}
	pool := &ProviderPool{
		Platform:         "openai-chat",
		Name:             "Manual Models Mapping",
		Mode:             ProviderPoolModeManual,
		ManualProviderID: &providerID,
		Members:          []ProviderPoolMember{{ProviderID: providerID, Enabled: true}},
	}
	_, router, relayKey, _ := setupProviderPoolHTTPTest(t, "openai-chat", providers, pool)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+relayKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("manual models status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"public/model-one"`) ||
		!strings.Contains(w.Body.String(), `"id":"native"`) ||
		strings.Contains(w.Body.String(), "vendor/model-one") {
		t.Fatalf("manual mapped models response = %s", w.Body.String())
	}
}

func TestMapModelsListForProviderReturnsAllConcreteAliases(t *testing.T) {
	entries, err := parseModelsList([]byte(`{
		"object":"list",
		"data":[
			{"id":"vendor/shared","object":"model","owned_by":"vendor"},
			{"id":"native","object":"model"}
		]
	}`))
	if err != nil {
		t.Fatalf("parse models list: %v", err)
	}
	mapped, err := mapModelsListForProvider(entries, Provider{ModelMapping: map[string]string{
		"alias-b": "vendor/shared",
		"alias-a": "vendor/shared",
		"missing": "vendor/missing",
	}})
	if err != nil {
		t.Fatalf("map models list: %v", err)
	}
	body, err := marshalModelsList(mapped)
	if err != nil {
		t.Fatalf("marshal models list: %v", err)
	}

	var response struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode mapped models list: %v", err)
	}
	gotIDs := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		gotIDs = append(gotIDs, model.ID)
		if strings.HasPrefix(model.ID, "alias-") && model.OwnedBy != "vendor" {
			t.Fatalf("mapped alias %q did not preserve model fields", model.ID)
		}
	}
	if got, want := strings.Join(gotIDs, ","), "alias-a,alias-b,native"; got != want {
		t.Fatalf("mapped model IDs = %q, want %q", got, want)
	}
}

func TestCodexResponsesRequireManagedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamHits := 0
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++

		if r.Method != http.MethodPost {
			t.Errorf("期望 POST 请求，收到 %s", r.Method)
		}
		if r.URL.Path != "/responses" {
			t.Errorf("期望路径 /responses，收到 %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-api-key" {
			t.Errorf("上游 Authorization 头不正确，期望 'Bearer provider-api-key'，收到 %q", got)
		}
		if got := r.Header.Get(codexRelayKeyHeader); got != "" {
			t.Errorf("relay key 不应继续转发到上游，收到 %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("读取上游请求体失败: %v", err)
		}
		if string(body) != `{"model":"gpt-5-codex","input":"hello"}` {
			t.Errorf("Codex 请求体应原样透传，收到 %s", string(body))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
	}))
	defer upstreamServer.Close()

	providerService, relayService := newTestRelayService(t)
	err := providerService.SaveProviders("openai-responses", []Provider{
		{
			ID:             1,
			Name:           "CodexProvider",
			APIURL:         upstreamServer.URL,
			APIKey:         "provider-api-key",
			Enabled:        true,
			Level:          1,
			MaxConcurrency: defaultProviderMaxConcurrency,
		},
	})
	if err != nil {
		t.Fatalf("保存 codex provider 失败: %v", err)
	}

	relayKey, err := relayService.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("创建 Codex relay key 失败: %v", err)
	}
	homeDir, err := getUserHomeDir()
	if err != nil {
		t.Fatalf("获取测试 home 目录失败: %v", err)
	}
	codexDir := filepath.Join(homeDir, codexSettingsDir)
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatalf("创建 codex 配置目录失败: %v", err)
	}
	managedConfig := `model_provider = "code-switch-r"

[model_providers.code-switch-r]
name = "code-switch-r"
base_url = "` + RelayClientBaseURL(relayService.Addr()) + `"
wire_api = "responses"
`
	if err := os.WriteFile(filepath.Join(codexDir, codexConfigFileName), []byte(managedConfig), 0o600); err != nil {
		t.Fatalf("写入 codex 托管配置失败: %v", err)
	}

	// 确保默认池子存在且 relay key 已绑定（codex-settings 已写入，需要重新推导模式）
	if relayService.poolService != nil {
		// 更新默认池子为托管模式（codex-settings 已启用托管）
		pool, _ := relayService.poolService.GetPool("pool_openai-responses_default")
		if pool != nil {
			pool.Mode = ProviderPoolModeManaged
			// 更新成员：加入新创建的 provider
			pool.Members = []ProviderPoolMember{{ProviderID: 1, Enabled: true}}
			_, _ = relayService.poolService.SavePool(pool)
		}
		// 确保 relay key 绑定
		platformDefaults := map[string]string{
			"openai-responses": "pool_openai-responses_default",
			"openai-chat":      "pool_openai-chat_default",
			"claude":           "pool_claude_default",
		}
		_ = relayService.codexRelayKeys.EnsureDefaultPoolBindings(platformDefaults)
	}

	router := gin.New()
	relayService.registerRoutes(router)

	makeRequest := func(authHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := makeRequest(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("缺少 key 时应返回 401，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}

	if w := makeRequest("Bearer wrong-key"); w.Code != http.StatusUnauthorized {
		t.Fatalf("错误 key 时应返回 401，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}

	w := makeRequest("Bearer " + relayKey.Key)
	if w.Code != http.StatusOK {
		t.Fatalf("正确 key 时应返回 200，实际为 %d，响应体: %s", w.Code, w.Body.String())
	}

	if upstreamHits != 1 {
		t.Fatalf("只有合法请求才应命中上游，实际命中 %d 次", upstreamHits)
	}
}

func TestCodexEnableProxyWritesManagedRelayKey(t *testing.T) {
	setupRelayTestEnv(t)

	homeDir, err := getUserHomeDir()
	if err != nil {
		t.Fatalf("获取测试 home 目录失败: %v", err)
	}
	_ = os.Remove(filepath.Join(homeDir, ".code-switch", codexRelayKeysFile))
	_ = os.Remove(filepath.Join(homeDir, ".codex", "config.toml"))
	_ = os.Remove(filepath.Join(homeDir, ".codex", "auth.json"))

	relayKeys := NewCodexRelayKeyService()
	settings := NewCodexSettingsService("127.0.0.1:18100", relayKeys)
	if err := settings.EnableProxy(); err != nil {
		t.Fatalf("启用 Codex 代理失败: %v", err)
	}

	managedKey, err := relayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("获取 relay key 失败: %v", err)
	}

	authPath := filepath.Join(homeDir, ".codex", "auth.json")
	authData, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("读取 auth.json 失败: %v", err)
	}

	var payload map[string]string
	if err := json.Unmarshal(authData, &payload); err != nil {
		t.Fatalf("解析 auth.json 失败: %v", err)
	}

	if payload[codexEnvKey] != managedKey.Key {
		t.Fatalf("auth.json 中的 OPENAI_API_KEY = %q，期望 %q", payload[codexEnvKey], managedKey.Key)
	}
}

// TestModelsHandler_NoProviders 测试没有可用 provider 的情况
func TestModelsHandler_NoProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 创建空的 ProviderService
	providerService, relayService := newTestRelayService(t)
	if err := providerService.SaveProviders("claude", []Provider{}); err != nil {
		t.Fatalf("清空 claude provider 配置失败: %v", err)
	}

	// 创建测试路由
	router := gin.New()
	relayService.registerRoutes(router)

	relayKey, err := relayService.codexRelayKeys.EnsureDefaultKey()
	if err != nil {
		t.Fatalf("创建 relay key 失败: %v", err)
	}
	pool := &ProviderPool{
		Platform: "claude",
		Name:     "Empty Models Pool",
		Mode:     ProviderPoolModeManaged,
		Members:  []ProviderPoolMember{},
	}
	poolID, err := relayService.poolService.SavePool(pool)
	if err != nil {
		t.Fatalf("创建空模型列表测试池失败: %v", err)
	}
	if err := relayService.codexRelayKeys.SetPoolBinding(relayKey.ID, "claude", poolID); err != nil {
		t.Fatalf("绑定空模型列表测试池失败: %v", err)
	}

	// 创建测试请求
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+relayKey.Key)
	w := httptest.NewRecorder()

	// 执行请求
	router.ServeHTTP(w, req)

	// 验证响应状态码应该是 404（没有可用的 provider）
	if w.Code != http.StatusNotFound {
		t.Errorf("期望状态码 %d，收到 %d", http.StatusNotFound, w.Code)
	}

	// 验证响应包含错误信息
	var response map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &response)
	if err != nil {
		t.Errorf("响应体不是有效的 JSON: %v", err)
	}

	if _, ok := response["error"]; !ok {
		t.Error("响应缺少 'error' 字段")
	}
}
