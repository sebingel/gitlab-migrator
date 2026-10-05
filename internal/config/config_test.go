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
