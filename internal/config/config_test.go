package config

import (
	"os"
	"path/filepath"
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

func TestWarnings_PullRequestFlags(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *Config)
		want  []string
		// hint is the reason every warning of the case must end with.
		hint string
	}{
		{
			name:  "no pull request flags",
			setup: func(c *Config) {},
			want:  nil,
		},
		{
			name: "all flags without -migrate-pull-requests",
			setup: func(c *Config) {
				c.SkipInvalidMergeRequests = true
				c.SkipOpenMergeRequests = true
				c.StateDir = "./state"
				c.MergeRequestsAge = 365
			},
			want: []string{"-skip-invalid-merge-requests", "-skip-open-merge-requests", "-state-dir", "-merge-requests-max-age"},
			hint: "without -migrate-pull-requests",
		},
		{
			name: "all flags with -migrate-pull-requests",
			setup: func(c *Config) {
				c.EnablePullRequests = true
				c.SkipInvalidMergeRequests = true
				c.SkipOpenMergeRequests = true
				c.StateDir = "./state"
				c.MergeRequestsAge = 365
			},
			want: nil,
		},
		{
			name: "all flags with -pull-requests-only",
			setup: func(c *Config) {
				c.PullRequestsOnly = true
				c.SkipInvalidMergeRequests = true
				c.SkipOpenMergeRequests = true
				c.StateDir = "./state"
				c.MergeRequestsAge = 365
			},
			want: nil,
		},
		{
			name: "all flags with -report",
			setup: func(c *Config) {
				c.Report = true
				c.SkipInvalidMergeRequests = true
				c.SkipOpenMergeRequests = true
				c.StateDir = "./state"
				c.MergeRequestsAge = 365
			},
			want: []string{"-skip-invalid-merge-requests", "-state-dir"},
			hint: "with -report",
		},
		{
			// The report does not read these two flags, so
			// -migrate-pull-requests does not give them an effect.
			name: "all flags with -report and -migrate-pull-requests",
			setup: func(c *Config) {
				c.Report = true
				c.EnablePullRequests = true
				c.SkipInvalidMergeRequests = true
				c.SkipOpenMergeRequests = true
				c.StateDir = "./state"
				c.MergeRequestsAge = 365
			},
			want: []string{"-skip-invalid-merge-requests", "-state-dir"},
			hint: "with -report",
		},
		{
			name: "max age of 0 or less is no limit",
			setup: func(c *Config) {
				c.MergeRequestsAge = -1
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.setup(cfg)

			got := cfg.Warnings()
			if len(got) != len(tt.want) {
				t.Fatalf("Warnings() returned %d warnings %q, want %d naming %q", len(got), got, len(tt.want), tt.want)
			}
			for i, flagName := range tt.want {
				if !strings.HasPrefix(got[i], flagName+" ") {
					t.Errorf("warning %d is %q, want it to start with %q", i, got[i], flagName)
				}
				if !strings.HasSuffix(got[i], " "+tt.hint) {
					t.Errorf("warning %q does not end with %q", got[i], tt.hint)
				}
			}
		})
	}
}

// A flag set in the -config file must warn like the same flag on the command line.
func TestWarnings_FromConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"skip_invalid_merge_requests": true, "skip_open_merge_requests": true, "state_dir": "./state", "merge_requests_max_age": 30}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := validConfig()
	if err := cfg.LoadFile(path); err != nil {
		t.Fatalf("LoadFile() returned %v", err)
	}

	if got := cfg.Warnings(); len(got) != 4 {
		t.Errorf("Warnings() returned %q, want 4 warnings", got)
	}
}
