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
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"

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

// secondaryRateLimitBaseWait is the first wait for a secondary rate limit
// without rate limit headers. It doubles with each attempt.
const secondaryRateLimitBaseWait = 120 * time.Second

// secondaryRateLimitKey is the context key under which CheckRetry hands the
// parsed GitHubError of a secondary rate limit to Backoff.
type secondaryRateLimitKey struct{}

// markSecondaryRateLimit stores errResp in the context of resp.Request, so that
// Backoff can find it. retryablehttp v0.7.8 calls CheckRetry and then Backoff
// with the same resp, but drains resp.Body in between, and Backoff gets no
// context. The field resp.Request is replaced with a shallow copy that has the
// new context. The request itself is not changed, so the request that
// retryablehttp sends again is not affected. CheckRetry marks only responses
// that it retries. When retryablehttp gives up after the last attempt, it drains
// the body and returns no response (the client sets no ErrorHandler), so the
// marked resp.Request never reaches the caller.
//
// net/http always sets resp.Request. Without it there is nothing to carry the
// mark: markSecondaryRateLimit returns false, and Backoff uses the default wait.
func markSecondaryRateLimit(resp *http.Response, errResp GitHubError) bool {
	if resp.Request == nil {
		return false
	}
	ctx := context.WithValue(resp.Request.Context(), secondaryRateLimitKey{}, errResp)
	resp.Request = resp.Request.WithContext(ctx)
	return true
}

// secondaryRateLimitFrom returns the GitHubError that markSecondaryRateLimit
// stored in resp.Request.
func secondaryRateLimitFrom(resp *http.Response) (GitHubError, bool) {
	if resp.Request == nil {
		return GitHubError{}, false
	}
	errResp, ok := resp.Request.Context().Value(secondaryRateLimitKey{}).(GitHubError)
	return errResp, ok
}

// secondaryRateLimitWait returns the wait for a secondary rate limit without
// rate limit headers: secondaryRateLimitBaseWait * 2^attemptNum, capped at
// maxWait (DefaultBackoff also handles an overflow). It then adds 0 to 40 %
// jitter, never less, so that workers that hit the limit at the same time do
// not retry at the same time. The result is capped at maxWait plus its own 0 to
// 40 % jitter, so the waits at the cap are spread too. So the longest wait is
// 1.4 * maxWait. randFloat must return a number in [0, 1).
func secondaryRateLimitWait(maxWait time.Duration, attemptNum int, randFloat func() float64) time.Duration {
	sleep := retryablehttp.DefaultBackoff(secondaryRateLimitBaseWait, maxWait, attemptNum, nil)

	sleep += time.Duration(randFloat() * 0.4 * float64(sleep))

	if jitteredMax := maxWait + time.Duration(randFloat()*0.4*float64(maxWait)); sleep > jitteredMax {
		sleep = jitteredMax
	}

	return sleep
}

// buildRetryClient creates a retryable HTTP client with GitHub-specific backoff and retry logic.
func buildRetryClient(logger hclog.Logger) *retryablehttp.Client {
	return newRetryClient(logger, rand.Float64)
}

// newRetryClient is buildRetryClient with the random source for the jitter as a
// parameter, so tests can fix the jitter. randFloat must return a number in
// [0, 1), like rand.Float64. All workers share the client, so randFloat must be
// safe for concurrent use. rand.Float64 is; the Float64 method of a *rand.Rand
// is not.
func newRetryClient(logger hclog.Logger, randFloat func() float64) *retryablehttp.Client {
	// ErrorHandler stays nil on purpose: when retryablehttp gives up, it then
	// returns no response, so a resp.Request that markSecondaryRateLimit changed
	// never reaches the caller.
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
			jitter := time.Duration(randFloat() * 0.2 * float64(wait))
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

		// The order follows GitHub's advice for rate limits: Retry-After first,
		// then X-Ratelimit-Reset when X-Ratelimit-Remaining is 0, otherwise at
		// least one minute with an exponentially increasing wait for a secondary
		// rate limit. See
		// https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#exceeding-the-rate-limit
		//
		// Backoff does not read the body: retryablehttp drains up to 4096 bytes of
		// resp.Body after CheckRetry and before Backoff. CheckRetry hands a
		// secondary rate limit to Backoff through resp.Request instead, see
		// markSecondaryRateLimit.
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

		if errResp, ok := secondaryRateLimitFrom(resp); ok {
			sleep = secondaryRateLimitWait(max, attemptNum, randFloat)
			logger.Info("waiting for secondary rate limit recovery",
				"wait_duration", sleep,
				"attempt", attemptNum,
				"message", errResp.Message,
				"method", requestMethod,
				"url", requestUrl)
			return
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

		var errResp GitHubError
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			if errResp, err = parseGitHubError(resp); err != nil {
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

		if resp.StatusCode == http.StatusForbidden && strings.Contains(errResp.Message, "SAML enforcement") {
			msg := errResp.Message
			if errResp.DocumentationURL != "" {
				msg += fmt.Sprintf(" - %s", errResp.DocumentationURL)
			}
			return false, fmt.Errorf("received 403 with response: %v", msg)
		}

		// GitHub answers a secondary rate limit with 403 or 429. Both statuses
		// are retried, so the response is marked for the extended backoff in
		// Backoff.
		secondaryRateLimit := (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
			secondaryRateLimitPattern.MatchString(errResp.Message)
		if secondaryRateLimit {
			// A 403 is retried here. For a 403 the WARN line replaces the TRACE
			// line below. A 429 keeps its TRACE line.
			if resp.StatusCode == http.StatusForbidden {
				logger.Warn("secondary rate limit exceeded - will retry with extended backoff",
					"message", errResp.Message,
					"method", requestMethod,
					"url", requestUrl)
			}

			marked := markSecondaryRateLimit(resp, errResp)
			if !marked {
				logger.Warn("cannot hand the secondary rate limit to Backoff because the response has no request, using the default backoff instead",
					"status", resp.StatusCode)
			}

			if resp.StatusCode == http.StatusForbidden {
				return true, nil
			}
		}

		// Any other 403 is retried only for a primary rate limit. A 403 for a
		// missing permission does not go away by waiting, and with RetryMax and
		// RetryWaitMax a retried request would block its worker for hours.
		if resp.StatusCode == http.StatusForbidden && !isPrimaryRateLimit(resp.Header) {
			return false, nil
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

// isPrimaryRateLimit reports whether the headers of a response mark a primary
// rate limit: GitHub sends Retry-After, or X-RateLimit-Remaining 0. See
// https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#exceeding-the-rate-limit
func isPrimaryRateLimit(header http.Header) bool {
	if header.Get("Retry-After") != "" {
		return true
	}
	remaining, err := strconv.ParseInt(header.Get("X-RateLimit-Remaining"), 10, 64)
	return err == nil && remaining == 0
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
	// These patterns are based on the Go 1.25 net/http2 error strings. They were checked
	// again for Go 1.27.1, where this code is in net/http/internal/http2. A new minor Go
	// release can change them (see "New minor Go releases" in CONTRIBUTING.md).
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

// parseGitHubError reads the body of resp once and decodes it as a GitHubError.
// A leading UTF-8 byte order mark is removed, and an empty body gives an empty
// GitHubError. After a successful read, resp.Body is always replaced with a
// reader over the read bytes without the byte order mark, also when the body is
// empty or no JSON, so resp never keeps a closed body. When CheckRetry returns
// neither a retry nor an error (for example for a 404), the transports above the
// retry client and go-github get the body. In all other cases retryablehttp
// drains it.
func parseGitHubError(resp *http.Response) (GitHubError, error) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return GitHubError{}, fmt.Errorf("parsing response body: %+v", err)
	}
	_ = resp.Body.Close()

	respBody = bytes.TrimPrefix(respBody, []byte("\xef\xbb\xbf"))
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	var errResp GitHubError
	if len(respBody) == 0 {
		return errResp, nil
	}

	if err := json.Unmarshal(respBody, &errResp); err != nil {
		return GitHubError{}, fmt.Errorf("unmarshaling response body: %+v", err)
	}

	return errResp, nil
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
