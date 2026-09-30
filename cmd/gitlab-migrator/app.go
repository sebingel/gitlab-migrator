package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	gogitlab "github.com/xanzy/go-gitlab"

	"github.com/sebingel/gitlab-migrator/internal/clients"
	"github.com/sebingel/gitlab-migrator/internal/config"
	"github.com/sebingel/gitlab-migrator/internal/migration"
)

// GitHubError is the error body returned by the GitHub API.
type GitHubError struct {
	Message          string
	DocumentationURL string `json:"documentation_url"`
}

// App holds all runtime dependencies for the migration tool.
type App struct {
	migrator *migration.Migrator
}

// NewApp constructs an App by creating and wiring all runtime dependencies from the given config.
func NewApp(cfg *config.Config, logger hclog.Logger) (*App, error) {
	retryClient := buildRetryClient(logger)

	transport := &clients.SearchModder{
		Base: &retryablehttp.RoundTripper{Client: retryClient},
	}
	paginatedClient := githubpagination.NewClient(transport, githubpagination.WithPerPage(100))

	var gh *gogithub.Client
	if cfg.GithubDomain == config.DefaultGithubDomain {
		gh = gogithub.NewClient(paginatedClient).WithAuthToken(cfg.GithubToken)
	} else {
		githubURL := fmt.Sprintf("https://%s", cfg.GithubDomain)
		var err error
		if gh, err = gogithub.NewClient(paginatedClient).WithAuthToken(cfg.GithubToken).WithEnterpriseURLs(githubURL, githubURL); err != nil {
			return nil, fmt.Errorf("configuring GitHub enterprise client: %v", err)
		}
	}

	gitlabOpts := make([]gogitlab.ClientOptionFunc, 0)
	if cfg.GitlabDomain != config.DefaultGitlabDomain {
		gitlabURL := fmt.Sprintf("https://%s", cfg.GitlabDomain)
		gitlabOpts = append(gitlabOpts, gogitlab.WithBaseURL(gitlabURL))
	}
	gl, err := gogitlab.NewClient(cfg.GitlabToken, gitlabOpts...)
	if err != nil {
		return nil, fmt.Errorf("configuring GitLab client: %v", err)
	}

	ghClient := clients.NewGitHubClient(gh, logger)
	glClient := clients.NewGitLabClient(gl, logger)

	migrator := migration.NewMigrator(cfg, gh, gl, ghClient, glClient, logger)

	return &App{migrator: migrator}, nil
}

// Run performs the migration for the given projects.
func (a *App) Run(ctx context.Context, projects []migration.CSVRow, collector *migration.ResultCollector, sessionID string) error {
	return a.migrator.PerformMigration(ctx, projects, collector, sessionID)
}

// RunReport prints a migration report for the given projects without migrating.
func (a *App) RunReport(ctx context.Context, projects []migration.CSVRow) {
	a.migrator.PrintReport(ctx, projects)
}

var secondaryRateLimitPattern = regexp.MustCompile(`(?i)secondary rate limit|abuse detection|content creation`)

// buildRetryClient creates a retryable HTTP client with GitHub-specific backoff and retry logic.
func buildRetryClient(logger hclog.Logger) *retryablehttp.Client {
	retryClient := &retryablehttp.Client{
		HTTPClient:   cleanhttp.DefaultPooledClient(),
		Logger:       nil,
		RetryMax:     15,
		RetryWaitMin: 30 * time.Second,
		RetryWaitMax: 900 * time.Second,
	}

	retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) (sleep time.Duration) {
		if resp == nil {
			wait := retryablehttp.DefaultBackoff(min, max, attemptNum, nil)
			jitter := time.Duration(rand.Float64() * 0.2 * float64(wait))
			wait += jitter
			logger.Trace("waiting before retrying after network error", "sleep", wait, "attempt", attemptNum, "max_attempts", retryClient.RetryMax)
			return wait
		}

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

		// Backoff uses the status and the headers only. retryablehttp drains
		// resp.Body after CheckRetry and before Backoff, so the GitHub error
		// message cannot be read here.
		if s, ok := resp.Header["Retry-After"]; ok {
			if retryAfter, err := strconv.ParseInt(s[0], 10, 64); err == nil {
				sleep = time.Second * time.Duration(retryAfter)
				return
			}
		}

		if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
			if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && remaining == 0 {
				if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
					if recoveryEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
						sleep = roundDuration(time.Until(time.Unix(recoveryEpoch+30, 0)), time.Second)
						return
					}
				}

				sleep = 60 * time.Second
				return
			}
		}

		// resp is not passed on purpose: Retry-After in seconds was handled above,
		// and a Retry-After HTTP date is ignored, as before.
		sleep = retryablehttp.DefaultBackoff(min, max, attemptNum, nil)
		return
	}

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			if ctx.Err() != nil {
				return false, err
			}
			if isTransientNetworkError(err) {
				logger.Warn("transient network error - will retry", "error", err.Error())
				return true, nil
			}
			return false, err
		}

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

		if resp.StatusCode == http.StatusForbidden {
			if strings.Contains(errResp.Message, "SAML enforcement") {
				msg := errResp.Message
				if errResp.DocumentationURL != "" {
					msg += fmt.Sprintf(" - %s", errResp.DocumentationURL)
				}
				return false, fmt.Errorf("received 403 with response: %v", msg)
			}

			// The message only decides that the request is retried. Backoff
			// computes the wait from the status and the headers.
			if secondaryRateLimitPattern.MatchString(errResp.Message) {
				logger.Warn("secondary rate limit exceeded - will retry with extended backoff",
					"message", errResp.Message,
					"method", requestMethod,
					"url", requestUrl)
				return true, nil
			}
		}

		retryableStatuses := []int{
			http.StatusTooManyRequests,
			http.StatusForbidden,
			http.StatusRequestTimeout,
			http.StatusFailedDependency,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		}

		if slices.Contains(retryableStatuses, resp.StatusCode) {
			logger.Trace("retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "message", errResp.Message)
			return true, nil
		}

		return false, nil
	}

	return retryClient
}

func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}

	// Concrete type checks where possible
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		opMsg := strings.ToLower(opErr.Error())
		if strings.Contains(opMsg, "connection reset") || strings.Contains(opMsg, "broken pipe") {
			return true
		}
	}

	// String matching for HTTP/2-specific errors where no concrete type is exported by Go.
	// These patterns are based on Go 1.25 net/http2 error strings and may break on major updates.
	msg := strings.ToLower(err.Error())
	transientPatterns := []string{
		"stream error",
		"goaway",
		"use of closed network connection",
		"tls handshake timeout",
	}

	for _, pattern := range transientPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}

	return false
}

func unmarshalResp(resp *http.Response, model interface{}) error {
	if resp == nil {
		return nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("parsing response body: %+v", err)
	}
	_ = resp.Body.Close()

	respBody = bytes.TrimPrefix(respBody, []byte("\xef\xbb\xbf"))

	if len(respBody) == 0 {
		return nil
	}

	if err := json.Unmarshal(respBody, model); err != nil {
		return fmt.Errorf("unmarshaling response body: %+v", err)
	}

	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	return nil
}

func roundDuration(d, r time.Duration) time.Duration {
	if r <= 0 {
		return d
	}
	neg := d < 0
	if neg {
		d = -d
	}
	if m := d % r; m+m < r {
		d = d - m
	} else {
		d = d + r - m
	}
	if neg {
		return -d
	}
	return d
}
