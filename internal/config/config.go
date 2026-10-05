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
	UnarchiveArchivedRepos   bool `json:"unarchive_archived_repos,omitempty"`

	// Repository visibility for newly created GitHub repos
	RepoVisibility string `json:"repo_visibility,omitempty"`

	// Domain / connection settings
	GithubDomain string `json:"github_domain,omitempty"`
	GitlabDomain string `json:"gitlab_domain,omitempty"`

	// GithubToken and GitlabToken must be supplied via the GITHUB_TOKEN and
	// GITLAB_TOKEN environment variables respectively. They are intentionally
	// excluded from JSON config file support to avoid plaintext credentials in
	// configuration files that may be committed to version control.
	GithubToken string `json:"-"`
	GitlabToken string `json:"-"`

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

	// State persistence for resumption
	StateDir string `json:"state_dir,omitempty"`

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
//
// Token fields (github_token, gitlab_token) are rejected if found in the
// config file. Tokens must be supplied via environment variables only.
func (c *Config) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file %q: %w", path, err)
	}

	// Reject token fields in config files to prevent plaintext credential storage.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, found := raw["github_token"]; found {
			return fmt.Errorf("config file %q must not contain \"github_token\": use the GITHUB_TOKEN environment variable instead", path)
		}
		if _, found := raw["gitlab_token"]; found {
			return fmt.Errorf("config file %q must not contain \"gitlab_token\": use the GITLAB_TOKEN environment variable instead", path)
		}
	}

	if err := json.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parsing config file %q: %w", path, err)
	}
	return nil
}

// validateLogDirectory checks that -log-directory is used together with a
// -log-output that writes to a file. Normal mode and prepare mode share it.
func (c *Config) validateLogDirectory() error {
	if c.LogDirectory != "" && !strings.Contains(strings.ToLower(c.LogOutput), "file") {
		return fmt.Errorf("-log-directory requires -log-output to include 'file' (e.g. -log-output=file or -log-output=console,file)")
	}
	return nil
}

// Validate checks that the migration configuration is consistent and complete.
// It returns the first validation error encountered.
func (c *Config) Validate() error {
	if err := c.validateLogDirectory(); err != nil {
		return err
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

	if c.RepoVisibility != "private" && c.RepoVisibility != "internal" && c.RepoVisibility != "public" {
		return fmt.Errorf("repo-visibility must be one of 'private', 'internal', or 'public'")
	}

	// With 0 no worker starts and the migration never ends; a negative value
	// panics when the worker channel is made.
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("-max-concurrency must be at least 1, got %d", c.MaxConcurrency)
	}

	if c.PushBatchSize <= 0 {
		return fmt.Errorf("push-batch-size must be greater than 0")
	}

	return nil
}

// Warnings returns one message for each flag that is set but has no effect
// because pull requests are not migrated. They are not errors: the run can go
// on, but the user should know that the flag does nothing. -pull-requests-only
// implies -migrate-pull-requests. With -report, -skip-open-merge-requests and
// -merge-requests-max-age change the count, so they get no warning then.
func (c *Config) Warnings() []string {
	if c.EnablePullRequests || c.PullRequestsOnly {
		return nil
	}

	const suffix = " has no effect without -migrate-pull-requests"
	var warnings []string
	if c.SkipInvalidMergeRequests {
		warnings = append(warnings, "-skip-invalid-merge-requests"+suffix)
	}
	if c.SkipOpenMergeRequests && !c.Report {
		warnings = append(warnings, "-skip-open-merge-requests"+suffix)
	}
	if c.StateDir != "" {
		warnings = append(warnings, "-state-dir"+suffix)
	}
	// 0 or less means no limit, so such a value changes nothing anyway.
	if c.MergeRequestsAge > 0 && !c.Report {
		warnings = append(warnings, "-merge-requests-max-age"+suffix)
	}
	return warnings
}

// ValidatePrepare checks that prepare-mode configuration is consistent.
func (c *Config) ValidatePrepare() error {
	if err := c.validateLogDirectory(); err != nil {
		return err
	}
	if c.PrepareCloneURL == "" || c.PrepareTargetURL == "" {
		return fmt.Errorf("-prepare requires both -prepare-clone-url and -prepare-target-url")
	}
	if c.PrepareLargeFiles != "" && c.PrepareLargeFiles != "remove" && c.PrepareLargeFiles != "lfs" {
		return fmt.Errorf("-prepare-large-files must be 'remove' or 'lfs', got %q", c.PrepareLargeFiles)
	}
	if c.GithubRepo != "" || c.GitlabProject != "" || c.ProjectsCsvPath != "" {
		return fmt.Errorf("-prepare cannot be combined with -github-repo, -gitlab-project, or -projects-csv")
	}
	// 0 means automatic; a negative value gives an invalid batch size.
	if c.PrepareBatchCount < 0 {
		return fmt.Errorf("-prepare-batch-count must not be negative, got %d", c.PrepareBatchCount)
	}
	return nil
}
