package services

import "testing"

func TestDeepLinkImportProviderSetsDefaultMaxConcurrency(t *testing.T) {
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	providerService := NewProviderService()
	deepLinkService := NewDeepLinkService(providerService)

	_, err := deepLinkService.ImportProviderFromDeepLink(&DeepLinkImportRequest{
		App:      "openai-chat",
		Name:     "Imported Provider",
		Homepage: "https://provider.example.com",
		Endpoint: "https://api.provider.example.com",
		APIKey:   "sk-test",
	})
	if err != nil {
		t.Fatalf("ImportProviderFromDeepLink failed: %v", err)
	}

	providers, err := providerService.LoadProviders("openai-chat")
	if err != nil {
		t.Fatalf("LoadProviders failed: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	if providers[0].MaxConcurrency != defaultProviderMaxConcurrency {
		t.Fatalf("MaxConcurrency = %d, want %d", providers[0].MaxConcurrency, defaultProviderMaxConcurrency)
	}
}
