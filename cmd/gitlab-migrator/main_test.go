package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebingel/gitlab-migrator/internal/config"
)

// writeConfigFile writes content to a JSON file in a temp directory and
// returns its path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migration.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

func TestLoadConfig_ConfigFileOverridesMergeRequestsMaxAgeFlag(t *testing.T) {
	cfg := &config.Config{}
	path := writeConfigFile(t, `{"merge_requests_max_age": 90}`)

	if err := loadConfig(cfg, path, "30"); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.MergeRequestsAge != 90 {
		t.Errorf("MergeRequestsAge = %d, want 90 (the -config value must override the flag)", cfg.MergeRequestsAge)
	}
}

func TestLoadConfig_MergeRequestsMaxAgeFlagKeptWhenConfigFileLacksIt(t *testing.T) {
	cfg := &config.Config{}
	path := writeConfigFile(t, `{"migrate_pull_requests": true}`)

	if err := loadConfig(cfg, path, "30"); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.MergeRequestsAge != 30 {
		t.Errorf("MergeRequestsAge = %d, want 30 (the flag value must stay when the file does not set it)", cfg.MergeRequestsAge)
	}
}

func TestLoadConfig_InvalidMergeRequestsMaxAgeNamesTheFlag(t *testing.T) {
	cfg := &config.Config{}
	path := writeConfigFile(t, `{"merge_requests_max_age": 90}`)

	err := loadConfig(cfg, path, "abc")
	if err == nil {
		t.Fatal("loadConfig returned no error for a value that is not an integer")
	}
	if !strings.Contains(err.Error(), "-merge-requests-max-age") {
		t.Errorf("error %q does not name the -merge-requests-max-age flag", err)
	}
}

// A negative value is read like any integer by loadConfig, from the flag and
// from the file; Validate, which main runs next, rejects it.
func TestLoadConfig_NegativeMergeRequestsMaxAgeFailsValidation(t *testing.T) {
	tests := []struct {
		name string
		path string
		raw  string
	}{
		{name: "flag", raw: "-5"},
		{name: "config file", path: writeConfigFile(t, `{"merge_requests_max_age": -5}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				GithubRepo:     "org/repo",
				GitlabProject:  "group/project",
				StorageType:    "memory",
				RepoVisibility: "private",
				PushBatchSize:  1,
				MaxConcurrency: 4,
			}

			if err := loadConfig(cfg, tt.path, tt.raw); err != nil {
				t.Fatalf("loadConfig: %v", err)
			}

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate returned no error for a negative -merge-requests-max-age")
			}
			if !strings.Contains(err.Error(), "-merge-requests-max-age") {
				t.Errorf("error %q does not name the -merge-requests-max-age flag", err)
			}
		})
	}
}
