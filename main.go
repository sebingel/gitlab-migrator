package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/xanzy/go-gitlab"
)

const (
	dateFormat          = "Mon, 2 Jan 2006"
	defaultGithubDomain = "github.com"
	defaultGitlabDomain = "gitlab.com"
)

var loop, report, detailedReport bool
var deleteExistingRepos, enablePullRequests, noForce, renameMasterToMain, skipInvalidMergeRequests, trimGithubBranches bool
var githubDomain, githubRepo, githubToken, githubUser, gitlabDomain, gitlabProject, gitlabToken, projectsCsvPath, renameTrunkBranch, storageType, storageDir string
var logOutput, logDirectory string
var mergeRequestsAge, pushBatchSize int

var prepareMode bool
var prepareCloneURL, prepareTargetURL, prepareLargeFiles string
var prepareBatchCount int

var (
	cache          *objectCache
	errCount       int
	logger         hclog.Logger
	gh             *github.Client
	gl             *gitlab.Client
	maxConcurrency int
	version        = "development"
)

// Regex patterns compiled once at initialization
var secondaryRateLimitPattern = regexp.MustCompile(`(?i)secondary rate limit|abuse detection|content creation`)

type Project = []string

type Report struct {
	GroupName          string
	ProjectName        string
	MergeRequestsCount int
}

type GitHubError struct {
	Message          string
	DocumentationURL string `json:"documentation_url"`
}

func createLogWriter(logOutput, logDirectory, sessionID string) (io.Writer, *os.File, error) {
	// Default to console if empty
	if logOutput == "" {
		logOutput = "console"
	}

	// Parse comma-separated targets
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

	// Handle file output
	if hasFile {
		var targetDir string

		if logDirectory == "" {
			// Default: use "logs" subdirectory in executable directory
			exePath, err := os.Executable()
			if err != nil {
				return nil, nil, fmt.Errorf("getting executable path: %v", err)
			}
			exeDir := filepath.Dir(exePath)
			targetDir = filepath.Join(exeDir, "logs")

			// Create logs directory if it doesn't exist
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return nil, nil, fmt.Errorf("creating default log directory: %v", err)
			}
		} else {
			// Validate/create provided directory
			targetDir = logDirectory

			info, err := os.Stat(targetDir)
			if err != nil {
				if os.IsNotExist(err) {
					// Create directory if it doesn't exist
					if err := os.MkdirAll(targetDir, 0755); err != nil {
						return nil, nil, fmt.Errorf("creating log directory: %v", err)
					}
				} else {
					return nil, nil, fmt.Errorf("accessing log directory: %v", err)
				}
			} else {
				// Path exists - verify it's a directory
				if !info.IsDir() {
					return nil, nil, fmt.Errorf("log directory path is not a directory: %s", targetDir)
				}
			}
		}

		// Create session-specific filename using provided sessionID with collision retry
		fullPath := filepath.Join(targetDir, sessionID+"-gitlab-migrator.log")

		// O_EXCL = fail if file exists (atomic check+create)
		f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0666)
		if err != nil {
			if os.IsExist(err) {
				// File exists, try with counter suffix
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

		// Print log file location
		fmt.Fprintf(os.Stderr, "Logging to file: %s\n", fullPath)

		// Return appropriate writer
		if hasConsole {
			return io.MultiWriter(os.Stderr, f), f, nil
		}
		return f, f, nil
	}

	// Console only (default)
	return os.Stderr, nil, nil
}

func main() {
	var err error

	// Bypass pre-emptive rate limit checks in the GitHub client, as we will handle these via go-retryablehttp
	valueCtx := context.WithValue(context.Background(), github.BypassRateLimitCheck, true)

	// Assign a Done channel so we can abort on Ctrl-c
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

	var showVersion bool
	var mergeRequestsAgeRaw string
	fmt.Printf(fmt.Sprintf("gitlab-migrator %s\n", version))

	flag.BoolVar(&loop, "loop", false, "continue migrating until canceled")
	flag.BoolVar(&report, "report", false, "report on primitives to be migrated instead of beginning migration")
	flag.BoolVar(&detailedReport, "detailed-report", false, "write detailed migration report to reports/ directory")

	flag.BoolVar(&deleteExistingRepos, "delete-existing-repos", false, "whether existing repositories should be deleted before migrating")
	flag.BoolVar(&enablePullRequests, "migrate-pull-requests", false, "whether pull requests should be migrated")
	flag.BoolVar(&renameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)")
	flag.BoolVar(&skipInvalidMergeRequests, "skip-invalid-merge-requests", false, "when true, will log and skip invalid merge requests instead of raising an error")
	flag.BoolVar(&noForce, "no-force", false, "use regular push instead of force push (safe for repos where work has already begun)")
	flag.BoolVar(&trimGithubBranches, "trim-branches-on-github", false, "when true, will delete any branches on GitHub that are no longer present in GitLab")
	flag.BoolVar(&showVersion, "version", false, "output version information")

	flag.StringVar(&githubDomain, "github-domain", defaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "specifies the GitLab domain to use")
	flag.StringVar(&gitlabProject, "gitlab-project", "", "the GitLab project to migrate")
	flag.StringVar(&projectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-repo)")
	flag.StringVar(&mergeRequestsAgeRaw, "merge-requests-max-age", "", "optional maximum age in days of merge requests to migrate")
	flag.StringVar(&renameTrunkBranch, "rename-trunk-branch", "", "specifies the new trunk branch name (incompatible with -rename-master-to-main)")
	flag.StringVar(&logOutput, "log-output", "", "comma-separated log targets: console, file, or console,file (default: console)")
	flag.StringVar(&logDirectory, "log-directory", "", "directory for session log files (defaults to ./logs in executable directory)")
	flag.StringVar(&storageType, "storage-type", "memory", "git storage type: 'memory' or 'filesystem' (use filesystem for large repositories)")
	flag.StringVar(&storageDir, "storage-dir", "", "directory for filesystem storage (only used when -storage-type=filesystem, defaults to temp directory)")

	flag.IntVar(&maxConcurrency, "max-concurrency", 4, "how many projects to migrate in parallel")
	flag.IntVar(&pushBatchSize, "push-batch-size", math.MaxInt, "number of branches to push per batch (default: unlimited, use smaller values like 50-100 for large repos)")

	flag.BoolVar(&prepareMode, "prepare", false, "prepare mode: clone, clean large files, push to new remote (unattended)")
	flag.StringVar(&prepareCloneURL, "prepare-clone-url", "", "source repository clone URL (required with -prepare)")
	flag.StringVar(&prepareTargetURL, "prepare-target-url", "", "target repository push URL (required with -prepare)")
	flag.StringVar(&prepareLargeFiles, "prepare-large-files", "", "how to handle files >100MB: 'remove' (git-filter-repo) or 'lfs' (git lfs migrate)")
	flag.IntVar(&prepareBatchCount, "prepare-batch-count", 0, "override batch count for push (default: auto-calculate 10 batches/GB)")

	flag.Parse()

	if showVersion {
		return
	}

	// Validate log flag combination
	if logDirectory != "" && !strings.Contains(strings.ToLower(logOutput), "file") {
		fmt.Fprintf(os.Stderr, "Error: -log-directory requires -log-output to include 'file' (e.g. -log-output=file or -log-output=console,file)\n")
		os.Exit(1)
	}

	// Generate session ID for both logs and reports
	sessionID := time.Now().Format("2006-01-02-150405")

	// Create log writer based on flags
	logWriter, logFileHandle, err := createLogWriter(logOutput, logDirectory, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error setting up logging: %v\n", err)
		os.Exit(1)
	}

	// Ensure log file is closed on exit
	if logFileHandle != nil {
		defer logFileHandle.Close()
	}

	logger = hclog.New(&hclog.LoggerOptions{
		Name:   "gitlab-migrator",
		Level:  hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
		Output: logWriter,
	})

	cache = newObjectCache()

	// Handle prepare mode early — no API tokens needed
	if prepareMode {
		if prepareCloneURL == "" || prepareTargetURL == "" {
			logger.Error("-prepare requires both -prepare-clone-url and -prepare-target-url")
			os.Exit(1)
		}
		if prepareLargeFiles != "" && prepareLargeFiles != "remove" && prepareLargeFiles != "lfs" {
			logger.Error("-prepare-large-files must be 'remove' or 'lfs'", "value", prepareLargeFiles)
			os.Exit(1)
		}
		if githubRepo != "" || gitlabProject != "" || projectsCsvPath != "" {
			logger.Error("-prepare cannot be combined with -github-repo, -gitlab-project, or -projects-csv")
			os.Exit(1)
		}
		if !isAllowedCloneURL(prepareCloneURL) {
			logger.Error("-prepare-clone-url must use https:// or git@ SSH format", "url", prepareCloneURL)
			os.Exit(1)
		}
		if !isAllowedCloneURL(prepareTargetURL) {
			logger.Error("-prepare-target-url must use https:// or git@ SSH format", "url", prepareTargetURL)
			os.Exit(1)
		}
		if err := runPrepare(ctx, prepareCloneURL, prepareTargetURL, prepareLargeFiles, prepareBatchCount); err != nil {
			logger.Error("prepare failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Create result collector for migration reporting
	collector := newResultCollector()

	githubToken = os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		logger.Error("missing environment variable", "name", "GITHUB_TOKEN")
		os.Exit(1)
	}

	gitlabToken = os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		logger.Error("missing environment variable", "name", "GITLAB_TOKEN")
		os.Exit(1)
	}

	if githubUser == "" {
		githubUser = os.Getenv("GITHUB_USER")
	}

	if githubUser == "" {
		logger.Error("must specify GitHub user")
		os.Exit(1)
	}

	repoSpecifiedInline := githubRepo != "" && gitlabProject != ""
	if repoSpecifiedInline && projectsCsvPath != "" {
		logger.Error("cannot specify -projects-csv and either -github-repo or -gitlab-project at the same time")
		os.Exit(1)
	}
	if !repoSpecifiedInline && projectsCsvPath == "" {
		logger.Error("must specify either -projects-csv or both of -github-repo and -gitlab-project")
		os.Exit(1)
	}

	if renameMasterToMain && renameTrunkBranch != "" {
		logger.Error("cannot specify -rename-master-to-main and -rename-trunk-branch together")
		os.Exit(1)
	}

	if storageType != "memory" && storageType != "filesystem" {
		logger.Error("storage-type must be either 'memory' or 'filesystem'")
		os.Exit(1)
	}

	if pushBatchSize <= 0 {
		logger.Error("push-batch-size must be greater than 0")
		os.Exit(1)
	}

	if mergeRequestsAgeRaw != "" {
		if mergeRequestsAge, err = strconv.Atoi(mergeRequestsAgeRaw); err != nil {
			logger.Error("must specify an integer for -merge-requests-age")
			os.Exit(1)
		}
	}

	retryClient := &retryablehttp.Client{
		HTTPClient:   cleanhttp.DefaultPooledClient(),
		Logger:       nil,
		RetryMax:     15,
		RetryWaitMin: 30 * time.Second,
		RetryWaitMax: 900 * time.Second,
	}

	retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) (sleep time.Duration) {
		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		defer func() {
			logger.Trace("waiting before retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "sleep", sleep, "attempt", attemptNum, "max_attempts", retryClient.RetryMax)
		}()

		if resp != nil {
			var errResp GitHubError

			// Parse error response to detect secondary rate limit
			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
				_ = unmarshalResp(resp, &errResp)
			}

			// Check for secondary rate limit (no Retry-After header, needs longer wait)
			isSecondaryLimit := secondaryRateLimitPattern.MatchString(errResp.Message)

			// Check the Retry-After header
			if s, ok := resp.Header["Retry-After"]; ok {
				if retryAfter, err := strconv.ParseInt(s[0], 10, 64); err == nil {
					sleep = time.Second * time.Duration(retryAfter)
					return
				}
			}

			// If secondary rate limit without Retry-After, use extended wait
			if isSecondaryLimit {
				// Start with 2 minutes, double each retry (2min, 4min, 8min, 16min, ...)
				// Capped at max (5 minutes default, will be increased)
				baseWait := 120 * time.Second
				mult := math.Pow(2, float64(attemptNum))
				sleep = time.Duration(float64(baseWait) * mult)
				if sleep > max {
					sleep = max
				}

				// Add 0-40% jitter to prevent thundering herd when multiple workers
				// hit rate limits simultaneously (common with max-concurrency > 1)
				// IMPORTANT: Only ADD jitter, never subtract! We must never wait less than calculated.
				jitterPercent := rand.Float64() * 0.4 // Random value in [0, 0.4]
				jitter := time.Duration(jitterPercent * float64(sleep))
				sleep += jitter

				// Also jitter the max to prevent thundering herd at max boundary
				jitteredMax := max + time.Duration(rand.Float64()*0.4*float64(max))
				if sleep > jitteredMax {
					sleep = jitteredMax
				}

				message := errResp.Message
				if message == "" {
					message = "(unable to parse error response)"
				}

				logger.Info("waiting for secondary rate limit recovery",
					"wait_duration", sleep,
					"attempt", attemptNum,
					"message", message)
				return
			}

			// Reference:
			// - https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api?apiVersion=2022-11-28
			// - https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2022-11-28
			if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
				if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && remaining == 0 {

					// If x-ratelimit-reset is present, this indicates the UTC timestamp when we can retry
					if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
						if recoveryEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
							// Add 30 seconds to recovery timestamp for clock differences
							sleep = roundDuration(time.Until(time.Unix(recoveryEpoch+30, 0)), time.Second)
							return
						}
					}

					// Otherwise, wait for 60 seconds
					sleep = 60 * time.Second
					return
				}
			}
		}

		// Exponential backoff
		mult := math.Pow(2, float64(attemptNum)) * float64(min)
		wait := time.Duration(mult)
		if float64(wait) != mult || wait > max {
			wait = max
		}

		sleep = wait
		return
	}

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return false, err
		}

		// Potential connection reset
		if resp == nil {
			return true, nil
		}

		errResp := GitHubError{}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			if err = unmarshalResp(resp, &errResp); err != nil {
				return false, err
			}
		}

		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		// Token not authorized for org
		if resp.StatusCode == http.StatusForbidden {
			if match, err := regexp.MatchString("SAML enforcement", errResp.Message); err != nil {
				return false, fmt.Errorf("matching 403 response: %v", err)
			} else if match {
				msg := errResp.Message
				if errResp.DocumentationURL != "" {
					msg += fmt.Sprintf(" - %s", errResp.DocumentationURL)
				}
				return false, fmt.Errorf("received 403 with response: %v", msg)
			}

			// Detect secondary rate limit
			if secondaryRateLimitPattern.MatchString(errResp.Message) {
				logger.Warn("secondary rate limit exceeded - will retry with extended backoff",
					"message", errResp.Message,
					"method", requestMethod,
					"url", requestUrl)
				return true, nil
			}
		}

		retryableStatuses := []int{
			http.StatusTooManyRequests, // rate-limiting
			http.StatusForbidden,       // rate-limiting

			http.StatusRequestTimeout,
			http.StatusFailedDependency,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		}

		for _, status := range retryableStatuses {
			if resp.StatusCode == status {
				logger.Trace("retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "message", errResp.Message)
				return true, nil
			}
		}

		return false, nil
	}

	transport := &gitHubAdvancedSearchModder{
		base: &retryablehttp.RoundTripper{Client: retryClient},
	}
	client := githubpagination.NewClient(transport, githubpagination.WithPerPage(100))

	if githubDomain == defaultGithubDomain {
		gh = github.NewClient(client).WithAuthToken(githubToken)
	} else {
		githubUrl := fmt.Sprintf("https://%s", githubDomain)
		if gh, err = github.NewClient(client).WithAuthToken(githubToken).WithEnterpriseURLs(githubUrl, githubUrl); err != nil {
			sendErr(err)
			os.Exit(1)
		}
	}

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gitlabDomain != defaultGitlabDomain {
		gitlabUrl := fmt.Sprintf("https://%s", gitlabDomain)
		gitlabOpts = append(gitlabOpts, gitlab.WithBaseURL(gitlabUrl))
	}
	if gl, err = gitlab.NewClient(gitlabToken, gitlabOpts...); err != nil {
		sendErr(err)
		os.Exit(1)
	}

	projects := make([]Project, 0)
	if projectsCsvPath != "" {
		data, err := os.ReadFile(projectsCsvPath)
		if err != nil {
			sendErr(err)
			os.Exit(1)
		}

		// Trim a UTF-8 BOM, if present
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

		if projects, err = csv.NewReader(bytes.NewBuffer(data)).ReadAll(); err != nil {
			sendErr(err)
			os.Exit(1)
		}
	} else {
		projects = []Project{{gitlabProject, githubRepo}}
	}

	if report {
		printReport(ctx, projects)
	} else {
		if err = performMigration(ctx, projects, collector, sessionID); err != nil {
			sendErr(err)
			os.Exit(1)
		} else if errCount > 0 {
			logger.Warn(fmt.Sprintf("encountered %d errors during migration, review log output for details", errCount))
			os.Exit(1)
		}
	}
}

func printReport(ctx context.Context, projects []Project) {
	logger.Debug("building report")

	results := make([]Report, 0)

	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			return
		}

		result, err := reportProject(ctx, proj)
		if err != nil {
			errCount++
			sendErr(err)
		}

		if result != nil {
			results = append(results, *result)
		}
	}

	fmt.Println()

	totalMergeRequests := 0
	for _, result := range results {
		totalMergeRequests += result.MergeRequestsCount
		fmt.Printf("%#v\n", result)
	}

	fmt.Println()
	fmt.Printf("Total merge requests: %d\n", totalMergeRequests)
	fmt.Println()
}

func reportProject(_ context.Context, slugs []string) (*Report, error) {
	gitlabPath, _, err := parseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %v", err)
	}

	logger.Debug("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
	searchTerm := gitlabPath[1]
	projectResult, _, err := gl.Projects.ListProjects(&gitlab.ListProjectsOptions{Search: &searchTerm})
	if err != nil {
		return nil, fmt.Errorf("listing projects: %v", err)
	}

	var proj *gitlab.Project
	for _, item := range projectResult {
		if item == nil {
			continue
		}

		if item.PathWithNamespace == slugs[0] {
			logger.Debug("found GitLab project", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", item.ID)
			proj = item
		}
	}

	if proj == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	var mergeRequests []*gitlab.MergeRequest

	opts := &gitlab.ListProjectMergeRequestsOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", proj.ID)
	for {
		result, resp, err := gl.MergeRequests.ListProjectMergeRequests(proj.ID, opts)
		if err != nil {
			return nil, fmt.Errorf("retrieving gitlab merge requests: %v", err)
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return &Report{
		GroupName:          gitlabPath[0],
		ProjectName:        gitlabPath[1],
		MergeRequestsCount: len(mergeRequests),
	}, nil
}

func performMigration(ctx context.Context, projects []Project, collector *ResultCollector, sessionID string) error {
	concurrency := maxConcurrency
	if len(projects) < maxConcurrency {
		concurrency = len(projects)
	}

	logger.Info(fmt.Sprintf("processing %d project(s) with %d workers", len(projects), concurrency))

	var wg sync.WaitGroup
	queue := make(chan Project, concurrency*2)
	resultChan := make(chan ProjectResult, concurrency*2)

	// Launch collector goroutine
	collectorDone := make(chan bool)
	go func() {
		for result := range resultChan {
			collector.addProjectResult(result)
		}
		close(collectorDone)
	}()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for slugs := range queue {
				if err := ctx.Err(); err != nil {
					break
				}

				proj, err := newProject(slugs, storageType, storageDir, pushBatchSize)
				if err != nil {
					errCount++
					sendErr(err)
					// Send failure result
					gitlabPath, githubPath, parseErr := parseProjectSlugs(slugs)
					if parseErr != nil {
						gitlabPath = []string{"unknown", "unknown"}
						githubPath = []string{"unknown", "unknown"}
					}
					resultChan <- ProjectResult{
						GitLabGroup:   gitlabPath[0],
						GitLabProject: gitlabPath[1],
						GitHubOwner:   githubPath[0],
						GitHubRepo:    githubPath[1],
						Status:        StatusFailed,
						Error:         err.Error(),
						StartTime:     time.Now(),
						EndTime:       time.Now(),
					}
					continue
				}

				result, err := proj.migrate(ctx)
				if err != nil {
					errCount++
					sendErr(err)
					result.Status = StatusFailed
					result.Error = err.Error()
				}

				resultChan <- result
			}
		}()
	}

	queueProjects := func() {
		for _, proj := range projects {
			if err := ctx.Err(); err != nil {
				break
			}

			queue <- proj
		}
	}

	if loop {
		logger.Info("looping migration until canceled")
		for {
			if err := ctx.Err(); err != nil {
				break
			}

			queueProjects()
		}
	} else {
		queueProjects()
		close(queue)
	}

	wg.Wait()
	close(resultChan)
	<-collectorDone

	// Finalize report
	finalReport := collector.finalize()

	// Output detailed report if requested
	if detailedReport {
		// Create reports directory in executable directory (same pattern as logs)
		exePath, err := os.Executable()
		if err != nil {
			logger.Error("failed to get executable path for reports", "error", err)
		} else {
			exeDir := filepath.Dir(exePath)
			reportsDir := filepath.Join(exeDir, "reports")

			// Create reports directory if it doesn't exist
			if err := os.MkdirAll(reportsDir, 0755); err != nil {
				logger.Error("failed to create reports directory", "error", err)
			} else {
				// Write both JSON and Markdown reports with session ID
				jsonPath := filepath.Join(reportsDir, sessionID+"-migration-report.json")
				mdPath := filepath.Join(reportsDir, sessionID+"-migration-report.md")

				logger.Info("writing detailed migration reports", "directory", reportsDir, "session", sessionID)

				if err := writeJSONReport(finalReport, jsonPath); err != nil {
					logger.Error("failed to write JSON report", "error", err, "path", jsonPath)
				} else {
					logger.Info("JSON report written", "path", jsonPath)
				}

				if err := writeMarkdownReport(finalReport, mdPath); err != nil {
					logger.Error("failed to write Markdown report", "error", err, "path", mdPath)
				} else {
					logger.Info("Markdown report written", "path", mdPath)
				}
			}
		}
	}

	// Always print console summary
	printSummaryToConsole(finalReport)

	return nil
}
