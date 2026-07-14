package services

import "testing"

func TestAppSettingsDoNotContainPoolSpecificFirstTextRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := NewAppSettingsService(nil)
	settings, err := service.GetAppSettings()
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if _, err := service.SaveAppSettings(settings); err != nil {
		t.Fatalf("save app settings: %v", err)
	}
}
