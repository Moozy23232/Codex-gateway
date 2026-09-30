package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Recognize an existing native subscription login without initiating login or
// reading API credentials from the caller's environment.
func registerLoggedInOfficial(cfg *Config) bool {
	if _, err := readOfficialCredential(cfg); err != nil {
		return false
	}
	found, changed := false, false
	for name, provider := range cfg.Providers {
		if provider.Auth == "codex" && strings.TrimRight(provider.BaseURL, "/") == officialBaseURL {
			found = true
			if !provider.AutoModels {
				provider.AutoModels = true
				cfg.Providers[name] = provider
				changed = true
			}
		}
	}
	if found {
		return changed
	}
	name := "official"
	for suffix := 2; ; suffix++ {
		if _, exists := cfg.Providers[name]; !exists {
			break
		}
		name = "official_" + strconv.Itoa(suffix)
	}
	cfg.Providers[name] = Provider{BaseURL: officialBaseURL, Auth: "codex", AutoModels: true}
	return true
}

func hasAutomaticOfficial(cfg *Config) bool {
	for _, provider := range cfg.Providers {
		if provider.AutoModels && provider.Auth == "codex" {
			return true
		}
	}
	return false
}

// PrepareWrappedConfig keeps ordinary startup bounded. A cached model list is
// sufficient when offline; first-time official-only use bypasses the gateway.
func PrepareWrappedConfig(home string, cfg *Config) error {
	changed := registerLoggedInOfficial(cfg)
	if !hasAutomaticOfficial(cfg) {
		return nil
	}
	refresh := changed
	catalog, err := os.Stat(filepath.Join(home, "models.json"))
	if err != nil || time.Since(catalog.ModTime()) >= 10*time.Minute {
		refresh = true
	} else if auth, err := os.Stat(filepath.Join(cfg.CodexHome, "auth.json")); err == nil && auth.ModTime().After(catalog.ModTime()) {
		refresh = true
	}
	if !refresh {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// A native model refresh may be temporarily unavailable. Saving the retained
	// routes keeps third-party use working and bounds the retry interval.
	_, _ = SyncOfficialModels(ctx, home, cfg)
	return SaveConfig(home, cfg)
}

// Called only after the native login command has succeeded, never instead of it.
// A failed model refresh does not invalidate the successful native login.
func RefreshOfficialAfterLogin(home string, cfg *Config) error {
	changed := registerLoggedInOfficial(cfg)
	if !hasAutomaticOfficial(cfg) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, refreshErr := SyncOfficialModels(ctx, home, cfg)
	if changed || refreshErr == nil {
		if err := SaveConfig(home, cfg); err != nil {
			return err
		}
	}
	return refreshErr
}
