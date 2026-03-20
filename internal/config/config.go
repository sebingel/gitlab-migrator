package config

const (
	DateFormat          = "Mon, 2 Jan 2006"
	DefaultGithubDomain = "github.com"
	DefaultGitlabDomain = "gitlab.com"
)

// Config holds all runtime configuration for the migrator.
type Config struct {
	// Behavior flags
	Loop                     bool
	Report                   bool
	DetailedReport           bool
	DeleteExistingRepos      bool
	EnablePullRequests       bool
	NoForce                  bool
	PullRequestsOnly         bool
	RenameMasterToMain       bool
	SkipInvalidMergeRequests bool
	SkipOpenMergeRequests    bool
	TrimGithubBranches       bool

	// Domain / connection settings
	GithubDomain  string
	GitlabDomain  string

	// Auth
	GithubToken string
	GitlabToken string

	// Project targets
	GithubRepo      string
	GithubUser      string
	GitlabProject   string
	ProjectsCsvPath string

	// Branch rename options
	RenameTrunkBranch string

	// Logging
	LogOutput    string
	LogDirectory string

	// Storage
	StorageType string
	StorageDir  string

	// Concurrency / batching
	MaxConcurrency int
	PushBatchSize  int

	// MR age filter (days)
	MergeRequestsAge int

	// Prepare mode
	PrepareMode       bool
	PrepareCloneURL   string
	PrepareTargetURL  string
	PrepareLargeFiles string
	PrepareBatchCount int

	// Build-time version
	Version string
}
