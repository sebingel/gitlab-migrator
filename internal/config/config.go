package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	DateFormat          = "Mon, 2 Jan 2006"
	DefaultGithubDomain = "github.com"
	DefaultGitlabDomain = "gitlab.com"
)

// Config holds all runtime configuration for the migrator.
type Config struct {
	// Behavior flags
	Loop                     bool `json:"loop,omitempty"`
	Report                   bool `json:"report,omitempty"`
	DetailedReport           bool `json:"detailed_report,omitempty"`
	DeleteExistingRepos      bool `json:"delete_existing_repos,omitempty"`
	EnablePullRequests       bool `json:"migrate_pull_requests,omitempty"`
	NoForce                  bool `json:"no_force,omitempty"`
	PullRequestsOnly         bool `json:"pull_requests_only,omitempty"`
	RenameMasterToMain       bool `json:"rename_master_to_main,omitempty"`
	SkipInvalidMergeRequests bool `json:"skip_invalid_merge_requests,omitempty"`
	SkipOpenMergeRequests    bool `json:"skip_open_merge_requests,omitempty"`
	TrimGithubBranches       bool `json:"trim_branches_on_github,omitempty"`

	// Domain / connection settings
	GithubDomain string `json:"github_domain,omitempty"`
	GitlabDomain string `json:"gitlab_domain,omitempty"`

	// Auth (prefer environment variables; config file support provided for convenience)
	GithubToken string `json:"github_token,omitempty"`
	GitlabToken string `json:"gitlab_token,omitempty"`

	// Project targets
	GithubRepo      string `json:"github_repo,omitempty"`
	GithubUser      string `json:"github_user,omitempty"`
	GitlabProject   string `json:"gitlab_project,omitempty"`
	ProjectsCsvPath string `json:"projects_csv,omitempty"`

	// Branch rename options
	RenameTrunkBranch string `json:"rename_trunk_branch,omitempty"`

	// Logging
	LogOutput    string `json:"log_output,omitempty"`
	LogDirectory string `json:"log_directory,omitempty"`

	// Storage
	StorageType string `json:"storage_type,omitempty"`
	StorageDir  string `json:"storage_dir,omitempty"`

	// Concurrency / batching
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	PushBatchSize  int `json:"push_batch_size,omitempty"`

	// MR age filter (days)
	MergeRequestsAge int `json:"merge_requests_max_age,omitempty"`

	// Prepare mode
	PrepareMode       bool   `json:"prepare,omitempty"`
	PrepareCloneURL   string `json:"prepare_clone_url,omitempty"`
	PrepareTargetURL  string `json:"prepare_target_url,omitempty"`
	PrepareLargeFiles string `json:"prepare_large_files,omitempty"`
	PrepareBatchCount int    `json:"prepare_batch_count,omitempty"`

	// Build-time version (not configurable from file or flags)
	Version string `json:"-"`
}

// LoadFile reads configuration from a JSON file, merging values into c.
// Fields present in the file override the current values in c.
// Fields absent from the file are left unchanged.
func (c *Config) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file %q: %w", path, err)
	}
	if err := json.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parsing config file %q: %w", path, err)
	}
	return nil
}

// Validate checks that the migration configuration is consistent and complete.
// It returns the first validation error encountered.
func (c *Config) Validate() error {
	if c.LogDirectory != "" && !strings.Contains(strings.ToLower(c.LogOutput), "file") {
		return fmt.Errorf("-log-directory requires -log-output to include 'file' (e.g. -log-output=file or -log-output=console,file)")
	}

	repoSpecifiedInline := c.GithubRepo != "" && c.GitlabProject != ""
	if repoSpecifiedInline && c.ProjectsCsvPath != "" {
		return fmt.Errorf("cannot specify -projects-csv and either -github-repo or -gitlab-project at the same time")
	}
	if !repoSpecifiedInline && c.ProjectsCsvPath == "" {
		return fmt.Errorf("must specify either -projects-csv or both of -github-repo and -gitlab-project")
	}

	if c.RenameMasterToMain && c.RenameTrunkBranch != "" {
		return fmt.Errorf("cannot specify -rename-master-to-main and -rename-trunk-branch together")
	}

	if c.PullRequestsOnly {
		if c.DeleteExistingRepos {
			return fmt.Errorf("cannot specify -pull-requests-only and -delete-existing-repos together")
		}
		if c.RenameMasterToMain || c.RenameTrunkBranch != "" {
			return fmt.Errorf("cannot specify -pull-requests-only and branch rename options together")
		}
		if c.TrimGithubBranches {
			return fmt.Errorf("cannot specify -pull-requests-only and -trim-branches-on-github together")
		}
	}

	if c.StorageType != "memory" && c.StorageType != "filesystem" {
		return fmt.Errorf("storage-type must be either 'memory' or 'filesystem'")
	}

	if c.PushBatchSize <= 0 {
		return fmt.Errorf("push-batch-size must be greater than 0")
	}

	return nil
}

// ValidatePrepare checks that prepare-mode configuration is consistent.
func (c *Config) ValidatePrepare() error {
	if c.PrepareCloneURL == "" || c.PrepareTargetURL == "" {
		return fmt.Errorf("-prepare requires both -prepare-clone-url and -prepare-target-url")
	}
	if c.PrepareLargeFiles != "" && c.PrepareLargeFiles != "remove" && c.PrepareLargeFiles != "lfs" {
		return fmt.Errorf("-prepare-large-files must be 'remove' or 'lfs', got %q", c.PrepareLargeFiles)
	}
	if c.GithubRepo != "" || c.GitlabProject != "" || c.ProjectsCsvPath != "" {
		return fmt.Errorf("-prepare cannot be combined with -github-repo, -gitlab-project, or -projects-csv")
	}
	return nil
}
