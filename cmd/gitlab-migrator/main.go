package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"

	"github.com/manicminer/gitlab-migrator/internal/config"
	"github.com/manicminer/gitlab-migrator/internal/migration"
)

var version = "development"

func createLogWriter(logOutput, logDirectory, sessionID string) (io.Writer, *os.File, error) {
	if logOutput == "" {
		logOutput = "console"
	}

	targets := strings.Split(logOutput, ",")
	hasConsole := false
	hasFile := false

	for _, target := range targets {
		switch strings.TrimSpace(strings.ToLower(target)) {
		case "console":
			hasConsole = true
		case "file":
			hasFile = true
		default:
			return nil, nil, fmt.Errorf("invalid log target: %s (valid: console, file)", target)
		}
	}

	if hasFile {
		var targetDir string

		if logDirectory == "" {
			exePath, err := os.Executable()
			if err != nil {
				return nil, nil, fmt.Errorf("getting executable path: %v", err)
			}
			targetDir = filepath.Join(filepath.Dir(exePath), "logs")
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return nil, nil, fmt.Errorf("creating default log directory: %v", err)
			}
		} else {
			targetDir = logDirectory

			info, err := os.Stat(targetDir)
			if err != nil {
				if os.IsNotExist(err) {
					if err := os.MkdirAll(targetDir, 0755); err != nil {
						return nil, nil, fmt.Errorf("creating log directory: %v", err)
					}
				} else {
					return nil, nil, fmt.Errorf("accessing log directory: %v", err)
				}
			} else {
				if !info.IsDir() {
					return nil, nil, fmt.Errorf("log directory path is not a directory: %s", targetDir)
				}
			}
		}

		fullPath := filepath.Join(targetDir, sessionID+"-gitlab-migrator.log")

		f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0666)
		if err != nil {
			if os.IsExist(err) {
				for i := 2; i <= 10; i++ {
					fullPath = filepath.Join(targetDir, fmt.Sprintf("%s-%d-gitlab-migrator.log", sessionID, i))
					f, err = os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0666)
					if err == nil {
						break
					}
					if !os.IsExist(err) {
						return nil, nil, fmt.Errorf("opening log file: %v", err)
					}
				}
				if err != nil {
					return nil, nil, fmt.Errorf("failed to generate unique log filename after retries")
				}
			} else {
				return nil, nil, fmt.Errorf("opening log file: %v", err)
			}
		}

		fmt.Fprintf(os.Stderr, "Logging to file: %s\n", fullPath)

		if hasConsole {
			return io.MultiWriter(os.Stderr, f), f, nil
		}
		return f, f, nil
	}

	return os.Stderr, nil, nil
}

func main() {
	var err error

	valueCtx := context.WithValue(context.Background(), gogithub.BypassRateLimitCheck, true)

	ctx, cancel := context.WithCancel(valueCtx)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	cfg := &config.Config{Version: version}

	var showVersion bool
	var configPath string
	var mergeRequestsAgeRaw string
	fmt.Printf(fmt.Sprintf("gitlab-migrator %s\n", cfg.Version))

	flag.StringVar(&configPath, "config", "", "path to JSON configuration file (file values override command-line flags)")
	flag.BoolVar(&cfg.Loop, "loop", false, "continue migrating until canceled")
	flag.BoolVar(&cfg.Report, "report", false, "report on primitives to be migrated instead of beginning migration")
	flag.BoolVar(&cfg.DetailedReport, "detailed-report", false, "write detailed migration report to reports/ directory")

	flag.BoolVar(&cfg.DeleteExistingRepos, "delete-existing-repos", false, "whether existing repositories should be deleted before migrating")
	flag.BoolVar(&cfg.EnablePullRequests, "migrate-pull-requests", false, "whether pull requests should be migrated")
	flag.BoolVar(&cfg.RenameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)")
	flag.BoolVar(&cfg.SkipInvalidMergeRequests, "skip-invalid-merge-requests", false, "when true, will log and skip invalid merge requests instead of raising an error")
	flag.BoolVar(&cfg.SkipOpenMergeRequests, "skip-open-merge-requests", false, "skip open merge requests during migration (only migrate closed/merged MRs)")
	flag.BoolVar(&cfg.PullRequestsOnly, "pull-requests-only", false, "migrate only closed/merged merge requests as pull requests without cloning/pushing the repository; open MRs are skipped (repo must already exist on GitHub)")
	flag.BoolVar(&cfg.NoForce, "no-force", false, "use regular push instead of force push (safe for repos where work has already begun)")
	flag.BoolVar(&cfg.TrimGithubBranches, "trim-branches-on-github", false, "when true, will delete any branches on GitHub that are no longer present in GitLab")
	flag.BoolVar(&showVersion, "version", false, "output version information")

	flag.StringVar(&cfg.GithubDomain, "github-domain", config.DefaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&cfg.GithubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&cfg.GithubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&cfg.GitlabDomain, "gitlab-domain", config.DefaultGitlabDomain, "specifies the GitLab domain to use")
	flag.StringVar(&cfg.GitlabProject, "gitlab-project", "", "the GitLab project to migrate")
	flag.StringVar(&cfg.ProjectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-repo)")
	flag.StringVar(&mergeRequestsAgeRaw, "merge-requests-max-age", "", "optional maximum age in days of merge requests to migrate")
	flag.StringVar(&cfg.RenameTrunkBranch, "rename-trunk-branch", "", "specifies the new trunk branch name (incompatible with -rename-master-to-main)")
	flag.StringVar(&cfg.LogOutput, "log-output", "", "comma-separated log targets: console, file, or console,file (default: console)")
	flag.StringVar(&cfg.LogDirectory, "log-directory", "", "directory for session log files (defaults to ./logs in executable directory)")
	flag.StringVar(&cfg.StorageType, "storage-type", "memory", "git storage type: 'memory' or 'filesystem' (use filesystem for large repositories)")
	flag.StringVar(&cfg.StorageDir, "storage-dir", "", "directory for filesystem storage (only used when -storage-type=filesystem, defaults to temp directory)")

	flag.IntVar(&cfg.MaxConcurrency, "max-concurrency", 4, "how many projects to migrate in parallel")
	flag.IntVar(&cfg.PushBatchSize, "push-batch-size", math.MaxInt, "number of branches to push per batch (default: unlimited, use smaller values like 50-100 for large repos)")

	flag.BoolVar(&cfg.PrepareMode, "prepare", false, "prepare mode: clone, clean large files, push to new remote (unattended)")
	flag.StringVar(&cfg.PrepareCloneURL, "prepare-clone-url", "", "source repository clone URL (required with -prepare)")
	flag.StringVar(&cfg.PrepareTargetURL, "prepare-target-url", "", "target repository push URL (required with -prepare)")
	flag.StringVar(&cfg.PrepareLargeFiles, "prepare-large-files", "", "how to handle files >100MB: 'remove' (git-filter-repo) or 'lfs' (git lfs migrate)")
	flag.IntVar(&cfg.PrepareBatchCount, "prepare-batch-count", 0, "override batch count for push (default: auto-calculate 10 batches/GB)")

	flag.Parse()

	if showVersion {
		return
	}

	if configPath != "" {
		if err = cfg.LoadFile(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	sessionID := time.Now().Format("2006-01-02-150405")

	logWriter, logFileHandle, err := createLogWriter(cfg.LogOutput, cfg.LogDirectory, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error setting up logging: %v\n", err)
		os.Exit(1)
	}

	if logFileHandle != nil {
		defer logFileHandle.Close()
	}

	logger := hclog.New(&hclog.LoggerOptions{
		Name:   "gitlab-migrator",
		Level:  hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
		Output: logWriter,
	})

	// Handle prepare mode early — no API tokens needed
	if cfg.PrepareMode {
		if err := cfg.ValidatePrepare(); err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
		if !isAllowedCloneURL(cfg.PrepareCloneURL) {
			logger.Error("-prepare-clone-url must use https:// or git@ SSH format", "url", cfg.PrepareCloneURL)
			os.Exit(1)
		}
		if !isAllowedCloneURL(cfg.PrepareTargetURL) {
			logger.Error("-prepare-target-url must use https:// or git@ SSH format", "url", cfg.PrepareTargetURL)
			os.Exit(1)
		}
		if err := newPreparer(logger).run(ctx, cfg.PrepareCloneURL, cfg.PrepareTargetURL, cfg.PrepareLargeFiles, cfg.PrepareBatchCount); err != nil {
			logger.Error("prepare failed", "error", err)
			os.Exit(1)
		}
		return
	}

	collector := migration.NewResultCollector()

	cfg.GithubToken = os.Getenv("GITHUB_TOKEN")
	if cfg.GithubToken == "" {
		logger.Error("missing environment variable", "name", "GITHUB_TOKEN")
		os.Exit(1)
	}

	cfg.GitlabToken = os.Getenv("GITLAB_TOKEN")
	if cfg.GitlabToken == "" {
		logger.Error("missing environment variable", "name", "GITLAB_TOKEN")
		os.Exit(1)
	}

	if cfg.GithubUser == "" {
		cfg.GithubUser = os.Getenv("GITHUB_USER")
	}

	if cfg.GithubUser == "" {
		logger.Error("must specify GitHub user")
		os.Exit(1)
	}

	if mergeRequestsAgeRaw != "" {
		if cfg.MergeRequestsAge, err = strconv.Atoi(mergeRequestsAgeRaw); err != nil {
			logger.Error("must specify an integer for -merge-requests-age")
			os.Exit(1)
		}
	}

	if cfg.PullRequestsOnly {
		cfg.EnablePullRequests = true
		cfg.SkipOpenMergeRequests = true
	}

	if err := cfg.Validate(); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	app, err := NewApp(cfg, logger)
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	projects := make([]migration.CSVRow, 0)
	if cfg.ProjectsCsvPath != "" {
		data, err := os.ReadFile(cfg.ProjectsCsvPath)
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}

		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

		if projects, err = csv.NewReader(bytes.NewBuffer(data)).ReadAll(); err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
	} else {
		projects = []migration.CSVRow{{cfg.GitlabProject, cfg.GithubRepo}}
	}

	if cfg.Report {
		app.RunReport(ctx, projects)
	} else {
		if err = app.Run(ctx, projects, collector, sessionID); err != nil {
			var partialErr *migration.MigrationPartialError
			if errors.As(err, &partialErr) {
				logger.Warn(partialErr.Error())
			} else {
				logger.Error(err.Error())
			}
			os.Exit(1)
		}
	}
}
