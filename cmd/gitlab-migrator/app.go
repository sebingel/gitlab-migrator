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
	paginatedClient := newGitHubHTTPClient(buildRetryClient(logger))

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

// newGitHubHTTPClient returns the HTTP client for go-github that NewApp uses:
// the pagination client over SearchModder over newRetryTransport. The tests of
// go-github requests use it too, so they check the same transport chain.
func newGitHubHTTPClient(retryClient *retryablehttp.Client) *http.Client {
	transport := &clients.SearchModder{
		Base: newRetryTransport(retryClient),
	}
	return clients.NewGitHubPaginationClient(transport, githubpagination.WithPerPage(100))
}

// Run performs the migration for the given projects.
func (a *App) Run(ctx context.Context, projects []migration.CSVRow, collector *migration.ResultCollector, sessionID string) error {
	return a.migrator.PerformMigration(ctx, projects, collector, sessionID)
}

// RunReport prints a migration report for the given projects without migrating.
// It returns an error when at least one project could not be reported.
func (a *App) RunReport(ctx context.Context, projects []migration.CSVRow) error {
	return a.migrator.PrintReport(ctx, projects)
}

// errNoResponseNoError is the error of CheckRetry for a call with neither a
// response nor an error.
var errNoResponseNoError = errors.New("retry check got neither a response nor an error")

var secondaryRateLimitPattern = regexp.MustCompile(`(?i)secondary rate limit|abuse detection|content creation`)

// secondaryRateLimitBaseWait is the first wait for a secondary rate limit
// without rate limit headers. It doubles with each further secondary rate limit
// of the request in a row, see secondaryRateLimitWait.
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

// secondaryRateLimitCountKey is the context key of the counter that
// secondaryRateLimitCounting gives each request.
type secondaryRateLimitCountKey struct{}

// secondaryRateLimitCounting is the transport between the callers and the
// retry client. It gives each request its own counter of the secondary rate
// limit responses in a row, in a context derived from the context of the
// request, so the caller's request is not changed. retryablehttp sends all
// attempts of the request with this context: CheckRetry gets it as ctx and
// counts there (countSecondaryRateLimit), and Backoff reads the count from
// resp.Request (secondaryRateLimitsInARow). So the extended backoff grows only
// while the secondary rate limit goes on, not with the other retries of the
// request before it. The counter is a plain int: retryablehttp calls CheckRetry
// and Backoff for the attempts of one request one after the other, in the
// goroutine of RoundTrip. The response that reaches the caller keeps this
// context in resp.Request; it holds only the counter.
type secondaryRateLimitCounting struct {
	Base http.RoundTripper
}

func (t *secondaryRateLimitCounting) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := context.WithValue(req.Context(), secondaryRateLimitCountKey{}, new(int))
	return t.Base.RoundTrip(req.WithContext(ctx))
}

// newRetryTransport returns the transport over the retry client that NewApp
// uses: secondaryRateLimitCounting over a retryablehttp.RoundTripper.
func newRetryTransport(retryClient *retryablehttp.Client) http.RoundTripper {
	return &secondaryRateLimitCounting{Base: &retryablehttp.RoundTripper{Client: retryClient}}
}

// countSecondaryRateLimit updates the counter of secondaryRateLimitCounting in
// ctx for one attempt: a secondary rate limit adds one, any other response or
// error sets it back to 0. Without a counter in ctx it does nothing.
func countSecondaryRateLimit(ctx context.Context, secondaryRateLimit bool) {
	count, ok := ctx.Value(secondaryRateLimitCountKey{}).(*int)
	if !ok {
		return
	}
	if secondaryRateLimit {
		*count++
	} else {
		*count = 0
	}
}

// secondaryRateLimitsInARow returns the number of secondary rate limit
// responses of the request in a row up to resp, resp included. ok is false
// when the request was not sent through secondaryRateLimitCounting.
func secondaryRateLimitsInARow(resp *http.Response) (count int, ok bool) {
	if resp.Request == nil {
		return 0, false
	}
	counter, ok := resp.Request.Context().Value(secondaryRateLimitCountKey{}).(*int)
	if !ok {
		return 0, false
	}
	return *counter, true
}

// secondaryRateLimitWait returns the wait for a secondary rate limit without
// rate limit headers: secondaryRateLimitBaseWait * 2^before, capped at maxWait
// (DefaultBackoff also handles an overflow). before is the number of secondary
// rate limit responses of the request right before this one, so the first one
// waits secondaryRateLimitBaseWait. It then adds 0 to 40 % jitter, never less,
// so that workers that hit the limit at the same time do not retry at the same
// time. The result is capped at maxWait plus its own 0 to 40 % jitter, so the
// waits at the cap are spread too. So the longest wait is 1.4 * maxWait.
// randFloat must return a number in [0, 1).
func secondaryRateLimitWait(maxWait time.Duration, before int, randFloat func() float64) time.Duration {
	sleep := retryablehttp.DefaultBackoff(secondaryRateLimitBaseWait, maxWait, before, nil)

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

		// waitReason names what decided the wait of a secondary rate limit, for
		// its WARN line. retryablehttp calls Backoff only when a retry follows, so
		// the line is only logged then, and it says which wait really happens:
		// the header based wait or the extended backoff.
		var waitReason string
		defer func() {
			if errResp, ok := secondaryRateLimitFrom(resp); ok {
				logger.Warn("secondary rate limit exceeded, waiting before the retry",
					"wait_duration", sleep,
					"wait_reason", waitReason,
					"attempt", attemptNum,
					"status", resp.StatusCode,
					"message", errResp.Message,
					"method", requestMethod,
					"url", requestUrl)
			}
			logger.Trace("waiting before retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "sleep", sleep, "attempt", attemptNum, "max_attempts", retryClient.RetryMax)
		}()

		// The order follows GitHub's advice for rate limits: Retry-After first,
		// then X-Ratelimit-Reset when X-Ratelimit-Remaining is 0, otherwise at
		// least one minute with an exponentially increasing wait for a secondary
		// rate limit. See
		// https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#exceeding-the-rate-limit
		// The waits from Retry-After and X-Ratelimit-Reset are capped at max
		// (RetryWaitMax), see retryAfterWait and rateLimitResetWait.
		//
		// Backoff does not read the body: retryablehttp drains up to 4096 bytes of
		// resp.Body after CheckRetry and before Backoff. CheckRetry hands a
		// secondary rate limit to Backoff through resp.Request instead, see
		// markSecondaryRateLimit.
		if s, ok := resp.Header["Retry-After"]; ok {
			if wait, ok := retryAfterWait(s[0], min, max, time.Now()); ok {
				sleep = wait
				waitReason = "Retry-After header"
				return
			}
		}

		if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
			if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && remaining == 0 {
				if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
					if resetEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
						sleep = rateLimitResetWait(resetEpoch, min, max, time.Now())
						waitReason = "X-Ratelimit-Reset header"
						return
					}
				}

				sleep = 60 * time.Second
				waitReason = "X-Ratelimit-Remaining 0 without a valid X-Ratelimit-Reset"
				return
			}
		}

		if _, ok := secondaryRateLimitFrom(resp); ok {
			// CheckRetry counted resp before it marked it, so count is at least 1.
			before := attemptNum
			if count, ok := secondaryRateLimitsInARow(resp); ok {
				before = count - 1
			} else {
				logger.Warn("cannot count the secondary rate limits of the request because it was not sent through secondaryRateLimitCounting, using the attempt number instead",
					"attempt", attemptNum,
					"method", requestMethod,
					"url", requestUrl)
			}
			sleep = secondaryRateLimitWait(max, before, randFloat)
			waitReason = "extended backoff"
			return
		}

		// resp is not passed on purpose: Retry-After in both forms was handled
		// above with a cap, and DefaultBackoff would use it again without one. A
		// Retry-After value that retryAfterWait cannot read gets the default
		// backoff.
		sleep = retryablehttp.DefaultBackoff(min, max, attemptNum, nil)
		return
	}

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			countSecondaryRateLimit(ctx, false)
			if ctx.Err() != nil {
				return false, err
			}
			if isTransientNetworkError(err) {
				logger.Warn("transient network error - will retry", "error", err.Error())
				return true, nil
			}
			return false, err
		}

		// net/http never returns no response and no error. If it did, a retry
		// would hide the defect, so the request fails with an error that names it.
		if resp == nil {
			return false, errNoResponseNoError
		}

		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		// A 4xx body that is no JSON (for example an HTML page of a proxy) or
		// that cannot be read gives no message. The status still decides about
		// the retry, and a response that is not retried reaches go-github, which
		// builds its error from the status and the body.
		var errResp GitHubError
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			var parseErr error
			if errResp, parseErr = parseGitHubError(resp); parseErr != nil {
				logger.Warn("cannot parse the error body of the response, going on without its message",
					"method", requestMethod,
					"url", requestUrl,
					"status", resp.StatusCode,
					"error", parseErr)
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
		// Every response that is retried comes past this line, so the count of
		// secondary rate limits in a row is right for the next attempt. A secondary
		// rate limit with Retry-After counts too: the limit goes on.
		countSecondaryRateLimit(ctx, secondaryRateLimit)
		if secondaryRateLimit {
			// CheckRetry does not know whether a retry follows: after the last
			// attempt retryablehttp gives up. So this line is TRACE and does not
			// say "retry". Backoff runs only before a retry and logs the WARN line
			// with the wait that really happens (issues #87 and #88).
			logger.Trace("secondary rate limit exceeded",
				"status", resp.StatusCode,
				"message", errResp.Message,
				"method", requestMethod,
				"url", requestUrl)

			marked := markSecondaryRateLimit(resp, errResp)
			if !marked {
				logger.Warn("cannot hand the secondary rate limit to Backoff because the response has no request, using the default backoff instead",
					"status", resp.StatusCode)
			}

			// 403 and 429 are both rate limits, which are retried for all methods.
			return true, nil
		}

		// Any other 403 is retried only for a primary rate limit. A 403 for a
		// missing permission does not go away by waiting, and with RetryMax and
		// RetryWaitMax a retried request would block its worker for hours.
		if resp.StatusCode == http.StatusForbidden && !isPrimaryRateLimit(resp.Header) {
			return false, nil
		}

		// GitHub can process a request and still answer with a 5xx, for example
		// create a pull request or a comment and then time out. A retry of a POST
		// or PATCH would then create a duplicate or apply the change twice, so these
		// methods are not retried after a 5xx. The caller gets the response.
		// Rate limits (403 and 429) are no 5xx, so they are still retried for all
		// methods: GitHub did not process those requests.
		if resp.StatusCode >= 500 && isNonIdempotentMethod(requestMethod) {
			logger.Warn("server error for a non-idempotent request - will not retry, because GitHub may have processed it already and a retry could create a duplicate",
				"method", requestMethod,
				"url", requestUrl,
				"status", resp.StatusCode)
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

// retryAfterWait returns the wait that a Retry-After value asks for, capped at
// maxWait, so that one response cannot block a worker for longer than the
// longest wait of Backoff. RFC 9110 allows two forms: a number of seconds, or an
// HTTP date. A date gives the time from now until that date, rounded up to
// seconds so that the retry is not sent before that date, and at least minWait:
// the date depends on the clock of the server, so a date in the past (for
// example when the clock of this machine is ahead) does not let all retries run
// at once. A number of seconds does not depend on a clock and
// has no lower bound. ok is false when the value is neither a number of seconds
// nor an HTTP date, or a negative number (the RFC allows only digits), so
// Backoff never waits a negative time. The cap is checked before the seconds
// become a time.Duration, so a very large value does not overflow.
func retryAfterWait(value string, minWait, maxWait time.Duration, now time.Time) (wait time.Duration, ok bool) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(maxWait/time.Second) {
			return maxWait, true
		}
		return time.Duration(seconds) * time.Second, true
	}

	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return clampAndCeil(date.Sub(now), minWait, maxWait), true
}

// clampAndCeil bounds d to [minWait, maxWait] and then rounds it up to seconds,
// so that a wait never ends before the time it was computed from. The bounds
// come first: Time.Sub saturates at the smallest or largest time.Duration for a
// time far away, and rounding up would overflow for the largest one. minWait and
// maxWait are whole seconds, so rounding up keeps the result inside the bounds.
func clampAndCeil(d, minWait, maxWait time.Duration) time.Duration {
	d = min(max(d, minWait), maxWait)
	if rest := d % time.Second; rest > 0 {
		d += time.Second - rest
	}
	return d
}

// rateLimitResetWait returns the wait until 30 s after the X-Ratelimit-Reset
// time resetEpoch (in seconds since the Unix epoch), rounded up to seconds. The
// wait is capped at maxWait, so that one response cannot block a worker for
// longer than the longest wait of Backoff; GitHub then answers the retry with
// the same reset time, and Backoff waits again. It is at least minWait, which is
// also the wait for a reset time of now: a reset time in the past (for example
// when the clock of this machine is ahead of GitHub's) does not give a zero or
// negative wait, so the retry does not hit the rate limit again at once.
func rateLimitResetWait(resetEpoch int64, minWait, maxWait time.Duration, now time.Time) time.Duration {
	return clampAndCeil(time.Unix(resetEpoch, 0).Add(30*time.Second).Sub(now), minWait, maxWait)
}

// isNonIdempotentMethod reports whether a request with this method can change
// the state on the server again when it is sent twice. POST creates a new item
// each time, and HTTP does not define PATCH as idempotent (RFC 5789). GET,
// HEAD, PUT and DELETE are idempotent (RFC 9110, section 9.2.2). A method that
// is not known ("unknown" when the response has no request) counts as
// idempotent, so it is retried as before.
func isNonIdempotentMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch
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
// GitHubError. resp.Body is always replaced with a reader over the read bytes
// without the byte order mark, also when the body is empty, no JSON, or cannot
// be read to the end (then the reader has the bytes read before the error), so
// resp never keeps a closed body. When CheckRetry does not retry (for example
// for a 404), the transports above the retry client and go-github get the body.
// Otherwise retryablehttp drains it. The returned error wraps the read or
// decode error.
func parseGitHubError(resp *http.Response) (GitHubError, error) {
	respBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	respBody = bytes.TrimPrefix(respBody, []byte("\xef\xbb\xbf"))
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	if readErr != nil {
		return GitHubError{}, fmt.Errorf("reading response body: %w", readErr)
	}

	var errResp GitHubError
	if len(respBody) == 0 {
		return errResp, nil
	}

	if err := json.Unmarshal(respBody, &errResp); err != nil {
		return GitHubError{}, fmt.Errorf("unmarshaling response body: %w", err)
	}

	return errResp, nil
}
