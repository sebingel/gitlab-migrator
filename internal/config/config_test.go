package config

import (
	"strings"
	"testing"
)

// validConfig returns a configuration that passes Validate.
func validConfig() *Config {
	return &Config{
		GithubRepo:     "org/repo",
		GitlabProject:  "group/project",
		StorageType:    "memory",
		RepoVisibility: "private",
		PushBatchSize:  1,
		MaxConcurrency: 4,
	}
}

func TestValidate_MaxConcurrency(t *testing.T) {
	tests := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{name: "negative", value: -1, wantErr: true},
		{name: "zero", value: 0, wantErr: true},
		{name: "one", value: 1, wantErr: false},
		{name: "default", value: 4, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.MaxConcurrency = tt.value

			err := cfg.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate() returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() returned nil for max-concurrency %d, want an error", tt.value)
			}
			if !strings.Contains(err.Error(), "-max-concurrency") {
				t.Errorf("error %q does not name -max-concurrency", err)
			}
		})
	}
}

// validPrepareConfig returns a configuration that passes ValidatePrepare.
func validPrepareConfig() *Config {
	return &Config{
		PrepareMode:      true,
		PrepareCloneURL:  "https://gitlab.example.com/group/repo.git",
		PrepareTargetURL: "https://github.com/org/repo.git",
	}
}

func TestValidatePrepare_BatchCount(t *testing.T) {
	tests := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{name: "negative", value: -1, wantErr: true},
		{name: "zero is automatic", value: 0, wantErr: false},
		{name: "positive", value: 10, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validPrepareConfig()
			cfg.PrepareBatchCount = tt.value

			err := cfg.ValidatePrepare()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidatePrepare() returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePrepare() returned nil for prepare-batch-count %d, want an error", tt.value)
			}
			if !strings.Contains(err.Error(), "-prepare-batch-count") {
				t.Errorf("error %q does not name -prepare-batch-count", err)
			}
		})
	}
}

func TestValidatePrepare_LogDirectory(t *testing.T) {
	tests := []struct {
		name         string
		logOutput    string
		logDirectory string
		wantErr      bool
	}{
		{name: "directory without file output", logOutput: "console", logDirectory: "logs", wantErr: true},
		{name: "directory with empty output", logOutput: "", logDirectory: "logs", wantErr: true},
		{name: "directory with file output", logOutput: "file", logDirectory: "logs", wantErr: false},
		{name: "directory with console and file output", logOutput: "console,file", logDirectory: "logs", wantErr: false},
		{name: "no directory", logOutput: "console", logDirectory: "", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validPrepareConfig()
			cfg.LogOutput = tt.logOutput
			cfg.LogDirectory = tt.logDirectory

			err := cfg.ValidatePrepare()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidatePrepare() returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePrepare() returned nil for -log-directory with -log-output %q, want an error", tt.logOutput)
			}
			if !strings.Contains(err.Error(), "-log-directory") {
				t.Errorf("error %q does not name -log-directory", err)
			}
		})
	}
}
