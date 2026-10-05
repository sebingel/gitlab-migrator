package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"

	"github.com/sebingel/gitlab-migrator/internal/clients"
)

const secondaryRateLimitBody = `{"message":"You have exceeded a secondary rate limit and have been temporarily blocked from content creation.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`

// retryHarness runs requests through the retry client from newRetryClient (the
// client of buildRetryClient and NewApp), so CheckRetry and Backoff run in the
// order that retryablehttp uses.
type retryHarness struct {
	client *retryablehttp.Client
	logs   *bytes.Buffer
	// waits holds every wait that Backoff computed, in call order.
	waits []time.Duration
}

// newRetryHarness builds the client with randFloat as the random source for the
// jitter: rand.Float64 as in buildRetryClient, or a fixed source. It wraps
// Backoff: the wrapper records the computed wait and returns 0, so the tests do
// not sleep. The Backoff log lines still show the computed wait.
func newRetryHarness(t *testing.T, randFloat func() float64) *retryHarness {
	t.Helper()

	h := &retryHarness{logs: &bytes.Buffer{}}
	logger := hclog.New(&hclog.LoggerOptions{
		Level:       hclog.Trace,
		Output:      h.logs,
		DisableTime: true,
	})
	h.client = newRetryClient(logger, randFloat)

	backoff := h.client.Backoff
	h.client.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
		h.waits = append(h.waits, backoff(min, max, attemptNum, resp))
		return 0
	}

	return h
}

// logLines returns the captured log lines with the test server URL replaced by
// "SERVER", or nil when nothing was logged.
func (h *retryHarness) logLines(serverURL string) []string {
	out := strings.ReplaceAll(h.logs.String(), serverURL, "SERVER")
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// expectResponse sends a GET through the retry client, expects a response with
// the given status and returns its body.
func (h *retryHarness) expectResponse(t *testing.T, target string, status int) string {
	t.Helper()
	resp, err := h.client.Get(target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != status {
		t.Fatalf("got status %d with body %q, want %d", resp.StatusCode, body, status)
	}
	return body
}

// expectOK expects that the final response is 200 with the body "ok".
func (h *retryHarness) expectOK(t *testing.T, target string) {
	t.Helper()
	if body := h.expectResponse(t, target, http.StatusOK); body != "ok" {
		t.Fatalf("got body %q, want ok", body)
	}
}

// expectGiveUp expects no response and exactly the error want.
func (h *retryHarness) expectGiveUp(t *testing.T, target, want string) {
	t.Helper()
	resp, err := h.client.Get(target)
	if resp != nil {
		_ = resp.Body.Close()
		t.Errorf("got response with status %d, want nil", resp.StatusCode)
	}
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v\nwant %s", err, want)
	}
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d log lines, want %d\ngot:\n%s\nwant:\n%s", len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("log line %d:\ngot:  %s\nwant: %s", i, got[i], want[i])
		}
	}
}

func assertWaits(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("got waits %v, want %v", got, want)
	}
}

func assertCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	if got := calls.Load(); got != want {
		t.Errorf("got %d requests, want %d", got, want)
	}
}

// sequenceServer answers the n-th request with the n-th handler and fails the
// test when more requests arrive than handlers exist.
func sequenceServer(t *testing.T, handlers ...http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n > len(handlers) {
			t.Errorf("unexpected request %d to %s", n, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		handlers[n-1](w, r)
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

func respond(status int, header http.Header, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// respondEmptyStream sends the status and no body. With "Content-Length: 0"
// net/http would give the client http.NoBody, which can be read even after
// Close. So the handler flushes the headers first: the response is chunked,
// and the client gets a real body stream without content.
func respondEmptyStream(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.(http.Flusher).Flush()
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return string(b)
}

// fixedRand returns a random source for newRetryHarness that always
// returns v.
func fixedRand(v float64) func() float64 {
	return func() float64 { return v }
}

// A secondary rate limit without rate limit headers gets the extended backoff:
// 120 s for attempt 0, plus 0.25 * 0.4 = 10 % jitter. retryablehttp drains the
// body before Backoff runs, so CheckRetry hands the parsed message to Backoff.
func TestRetryClient_SecondaryRateLimit403(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, secondaryRateLimitBody),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, fixedRand(0.25))

	h.expectOK(t, srv.URL+"/repos/o/r/pulls")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{132 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[INFO]  waiting for secondary rate limit recovery: wait_duration=2m12s attempt=0 message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=2m12s attempt=0 max_attempts=15`,
	})
}

// GitHub answers a secondary rate limit with 403 or 429. A 429 with a secondary
// rate limit message gets the TRACE line (the WARN line is only for 403) and the
// extended backoff: 120 s plus 0.5 * 0.4 = 20 % jitter.
func TestRetryClient_SecondaryRateLimit429(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusTooManyRequests, nil, secondaryRateLimitBody),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, fixedRand(0.5))

	h.expectOK(t, srv.URL+"/repos/o/r/issues")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{144 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r/issues status=429 message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation."`,
		`[INFO]  waiting for secondary rate limit recovery: wait_duration=2m24s attempt=0 message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/issues`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/issues status=429 sleep=2m24s attempt=0 max_attempts=15`,
	})
}

func TestRetryClient_SecondaryRateLimitWithRetryAfter(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"Retry-After": {"45"}}, secondaryRateLimitBody),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, fixedRand(0.5))

	h.expectOK(t, srv.URL+"/repos/o/r/pulls")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{45 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=45s attempt=0 max_attempts=15`,
	})
}

// The extended backoff uses the attempt number of the request, also after other
// errors: a secondary rate limit at attempt 2 waits 120 s * 2^2 = 480 s. A 429
// secondary rate limit with Retry-After waits as the header says, without the
// INFO line.
func TestRetryClient_SecondaryRateLimitAfterOtherRetries(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusBadGateway, nil, "<html>bad gateway</html>"),
		respond(http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, secondaryRateLimitBody),
		respond(http.StatusForbidden, nil, secondaryRateLimitBody),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, fixedRand(0))

	h.expectOK(t, srv.URL+"/repos/o/r/pulls")

	assertCalls(t, calls, 4)
	assertWaits(t, h.waits, []time.Duration{30 * time.Second, 7 * time.Second, 480 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=502 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=502 sleep=30s attempt=0 max_attempts=15`,
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=429 message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation."`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=429 sleep=7s attempt=1 max_attempts=15`,
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[INFO]  waiting for secondary rate limit recovery: wait_duration=8m0s attempt=2 message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=8m0s attempt=2 max_attempts=15`,
	})
}

// The mark from CheckRetry belongs to one response. A plain 429 after a
// secondary rate limit gets the default backoff for attempt 1 (60 s), not the
// extended backoff (240 s).
func TestRetryClient_SecondaryRateLimitMarkDoesNotCarryOver(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, secondaryRateLimitBody),
		respond(http.StatusTooManyRequests, nil, `{"message":"API rate limit exceeded for user ID 1."}`),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, fixedRand(0))

	h.expectOK(t, srv.URL+"/repos/o/r/pulls")

	assertCalls(t, calls, 3)
	assertWaits(t, h.waits, []time.Duration{120 * time.Second, 60 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[INFO]  waiting for secondary rate limit recovery: wait_duration=2m0s attempt=0 message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=2m0s attempt=0 max_attempts=15`,
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=429 message="API rate limit exceeded for user ID 1."`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=429 sleep=1m0s attempt=1 max_attempts=15`,
	})
}

// The extended backoff doubles from 120 s per attempt and stops at RetryWaitMax
// (900 s); the jitter is 0 here. After the 16th attempt retryablehttp gives up
// without calling Backoff and returns no response.
func TestRetryClient_SecondaryRateLimitGivesUpAfterRetryMax(t *testing.T) {
	handlers := make([]http.HandlerFunc, 16)
	for i := range handlers {
		handlers[i] = respond(http.StatusForbidden, nil, secondaryRateLimitBody)
	}
	srv, calls := sequenceServer(t, handlers...)
	h := newRetryHarness(t, fixedRand(0))

	h.expectGiveUp(t, srv.URL+"/repos/o/r/pulls", "GET "+srv.URL+"/repos/o/r/pulls giving up after 16 attempt(s)")

	assertCalls(t, calls, 16)
	wantWaits := []time.Duration{120 * time.Second, 240 * time.Second, 480 * time.Second}
	for len(wantWaits) < 15 {
		wantWaits = append(wantWaits, 900*time.Second)
	}
	assertWaits(t, h.waits, wantWaits)

	const message = "You have exceeded a secondary rate limit and have been temporarily blocked from content creation."
	warn := `[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="` + message + `" method=GET url=SERVER/repos/o/r/pulls`
	var wantLines []string
	for i, wait := range wantWaits {
		wantLines = append(wantLines,
			warn,
			fmt.Sprintf(`[INFO]  waiting for secondary rate limit recovery: wait_duration=%s attempt=%d message="%s" method=GET url=SERVER/repos/o/r/pulls`, wait, i, message),
			fmt.Sprintf(`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=%s attempt=%d max_attempts=15`, wait, i),
		)
	}
	wantLines = append(wantLines, warn)
	assertLines(t, h.logLines(srv.URL), wantLines)
}

// net/http always sets resp.Request, so this cannot happen in the client. If it
// did, CheckRetry could not hand the secondary rate limit to Backoff. It says so
// in a WARN line, and Backoff uses the default wait.
func TestRetryClient_SecondaryRateLimitWithoutRequest(t *testing.T) {
	h := newRetryHarness(t, fixedRand(0.5))
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(secondaryRateLimitBody)),
	}

	if retry, err := h.client.CheckRetry(context.Background(), resp, nil); !retry || err != nil {
		t.Fatalf("got (%v, %v), want (true, nil)", retry, err)
	}
	h.client.Backoff(h.client.RetryWaitMin, h.client.RetryWaitMax, 0, resp)

	assertWaits(t, h.waits, []time.Duration{30 * time.Second})
	assertLines(t, h.logLines("unused"), []string{
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=unknown url=unknown`,
		`[WARN]  cannot hand the secondary rate limit to Backoff because the response has no request, using the default backoff instead: status=403`,
		`[TRACE] waiting before retrying failed API request: method=unknown url=unknown status=403 sleep=30s attempt=0 max_attempts=15`,
	})
}

// The rate limit headers come before the extended backoff, as GitHub advises:
// with X-Ratelimit-Remaining 0 and no X-Ratelimit-Reset Backoff waits 60 s and
// logs no INFO line. (The path with X-Ratelimit-Reset is the same code as in
// TestRetryClient_PrimaryRateLimitHeaders.)
func TestRetryClient_SecondaryRateLimitWithRateLimitHeaders(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, secondaryRateLimitBody),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, fixedRand(0.5))

	h.expectOK(t, srv.URL+"/repos/o/r/pulls")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{60 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=1m0s attempt=0 max_attempts=15`,
	})
}

// sequenceRand returns a random source that returns vals in order. It fails the
// test when it is called more often, or when values are left at the end.
func sequenceRand(t *testing.T, vals ...float64) func() float64 {
	t.Helper()
	t.Cleanup(func() {
		if len(vals) != 0 {
			t.Errorf("random source has %d unused values: %v", len(vals), vals)
		}
	})
	return func() float64 {
		t.Helper()
		if len(vals) == 0 {
			t.Fatal("random source called more often than expected")
		}
		v := vals[0]
		vals = vals[1:]
		return v
	}
}

// The first random value is the jitter on the wait, the second one the jitter
// on the cap (RetryWaitMax, 900 s).
func TestSecondaryRateLimitWait(t *testing.T) {
	const maxWait = 900 * time.Second
	tests := []struct {
		name    string
		attempt int
		rand    []float64
		want    time.Duration
	}{
		{name: "attempt 0 without jitter", attempt: 0, rand: []float64{0, 0}, want: 120 * time.Second},
		{name: "attempt 1 without jitter", attempt: 1, rand: []float64{0, 0}, want: 240 * time.Second},
		{name: "attempt 2 with 20 % jitter", attempt: 2, rand: []float64{0.5, 0}, want: 576 * time.Second},
		{name: "attempt 2 with almost 40 % jitter", attempt: 2, rand: []float64{0.99, 0}, want: 480*time.Second + 190080*time.Millisecond},
		{name: "attempt 3 is capped at max", attempt: 3, rand: []float64{0, 0}, want: maxWait},
		{name: "jitter above the jittered cap", attempt: 3, rand: []float64{0.5, 0.25}, want: 990 * time.Second},
		{name: "jitter below the jittered cap", attempt: 3, rand: []float64{0.25, 0.5}, want: 990 * time.Second},
		{name: "attempt 14 is capped at max", attempt: 14, rand: []float64{0, 0}, want: maxWait},
		{name: "huge attempt does not overflow", attempt: 1000, rand: []float64{0, 0}, want: maxWait},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secondaryRateLimitWait(maxWait, tt.attempt, sequenceRand(t, tt.rand...)); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryClient_SAMLEnforcement403(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, `{"message":"Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization.","documentation_url":"https://docs.github.com/articles/authenticating-to-a-github-organization-with-saml-single-sign-on/"}`),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectGiveUp(t, srv.URL+"/orgs/o/repos",
		"GET "+srv.URL+"/orgs/o/repos giving up after 1 attempt(s): received 403 with response: Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization. - https://docs.github.com/articles/authenticating-to-a-github-organization-with-saml-single-sign-on/")

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// A 403 without rate limit headers and without a secondary rate limit message,
// for example a missing permission, does not go away by waiting. It is not
// retried, and the caller gets the response with its body.
func TestRetryClient_PermissionError403(t *testing.T) {
	const body = `{"message":"Must have admin rights to Repository.","documentation_url":"https://docs.github.com/rest"}`
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"4999"}}, body),
	)
	h := newRetryHarness(t, rand.Float64)

	if got := h.expectResponse(t, srv.URL+"/repos/o/r/branches", http.StatusForbidden); got != body {
		t.Errorf("got body %q, want %q", got, body)
	}

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// Retry-After alone marks a 403 as a rate limit, also without a rate limit
// message, so it is retried and Backoff waits as the header says.
func TestRetryClient_ForbiddenWithRetryAfter(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"Retry-After": {"9"}}, `{"message":"Forbidden"}`),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/user")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{9 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/user status=403 message=Forbidden`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/user status=403 sleep=9s attempt=0 max_attempts=15`,
	})
}

func TestRetryClient_TooManyRequestsWithRetryAfter(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, `{"message":"API rate limit exceeded for user ID 1."}`),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/user")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{7 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/user status=429 message="API rate limit exceeded for user ID 1."`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/user status=429 sleep=7s attempt=0 max_attempts=15`,
	})
}

// CheckRetry does not parse 5xx bodies, so an HTML body is no error.
func TestRetryClient_ServerErrors(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusBadGateway, nil, "<html>bad gateway</html>"),
		respond(http.StatusServiceUnavailable, nil, "<html>unavailable</html>"),
		respond(http.StatusInternalServerError, nil, "<html>error</html>"),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/repos/o/r")

	assertCalls(t, calls, 4)
	assertWaits(t, h.waits, []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=502 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=502 sleep=30s attempt=0 max_attempts=15`,
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=503 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=503 sleep=1m0s attempt=1 max_attempts=15`,
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=500 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=500 sleep=2m0s attempt=2 max_attempts=15`,
	})
}

// 408 and 424 are the other 4xx statuses in retryableStatuses. CheckRetry parses
// their bodies, and they get the default backoff.
func TestRetryClient_RequestTimeoutAndFailedDependency(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusRequestTimeout, nil, `{"message":"Request Timeout"}`),
		respond(http.StatusFailedDependency, nil, `{"message":"Failed Dependency"}`),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/repos/o/r")

	assertCalls(t, calls, 3)
	assertWaits(t, h.waits, []time.Duration{30 * time.Second, 60 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=408 message="Request Timeout"`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=408 sleep=30s attempt=0 max_attempts=15`,
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=424 message="Failed Dependency"`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=424 sleep=1m0s attempt=1 max_attempts=15`,
	})
}

// A 4xx body that is no JSON stops the request, even for a retryable status.
func TestRetryClient_BadJSON4xx(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, "<html>forbidden</html>"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectGiveUp(t, srv.URL+"/repos/o/r",
		"GET "+srv.URL+"/repos/o/r giving up after 1 attempt(s): unmarshaling response body: invalid character '<' looking for beginning of value")

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// A retried 429 whose body stream is empty: CheckRetry reads it, and
// retryablehttp drains it again before Backoff.
func TestRetryClient_EmptyBody429(t *testing.T) {
	srv, calls := sequenceServer(t,
		respondEmptyStream(http.StatusTooManyRequests),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/repos/o/r")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{30 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=429 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=429 sleep=30s attempt=0 max_attempts=15`,
	})
}

func TestRetryClient_PrimaryRateLimitHeaders(t *testing.T) {
	const body = `{"message":"API rate limit exceeded for user ID 1."}`
	resetEpoch := time.Now().Unix() + 100
	reset := strconv.FormatInt(resetEpoch, 10)
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, body),
		respond(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, body),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	before := time.Now()
	h.expectOK(t, srv.URL+"/user")
	after := time.Now()

	assertCalls(t, calls, 3)
	if len(h.waits) != 2 {
		t.Fatalf("got waits %v, want 2 waits", h.waits)
	}
	// Backoff waits until 30 s after the reset time, rounded to seconds. It runs
	// between before and after, so its wait lies between these two bounds.
	recovery := time.Unix(resetEpoch+30, 0)
	lo, hi := recovery.Sub(after).Round(time.Second), recovery.Sub(before).Round(time.Second)
	if h.waits[0] < lo || h.waits[0] > hi {
		t.Errorf("wait 0: got %v, want between %v and %v", h.waits[0], lo, hi)
	}
	if h.waits[1] != 60*time.Second {
		t.Errorf("wait 1: got %v, want 1m0s", h.waits[1])
	}
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/user status=403 message="API rate limit exceeded for user ID 1."`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/user status=403 sleep=` + h.waits[0].String() + ` attempt=0 max_attempts=15`,
		`[TRACE] retrying failed API request: method=GET url=SERVER/user status=403 message="API rate limit exceeded for user ID 1."`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/user status=403 sleep=1m0s attempt=1 max_attempts=15`,
	})
}

func TestRetryClient_RetryAfterWinsOverRateLimitHeaders(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Unix()+600, 10)
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"Retry-After": {"5"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, `{"message":"API rate limit exceeded for user ID 1."}`),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/user")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{5 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/user status=403 message="API rate limit exceeded for user ID 1."`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/user status=403 sleep=5s attempt=0 max_attempts=15`,
	})
}

// Backoff reads Retry-After only in seconds. An HTTP date gives the default backoff.
func TestRetryClient_RetryAfterHTTPDateIsIgnored(t *testing.T) {
	date := time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat)
	srv, calls := sequenceServer(t,
		respond(http.StatusServiceUnavailable, http.Header{"Retry-After": {date}}, ""),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectOK(t, srv.URL+"/repos/o/r")

	assertCalls(t, calls, 2)
	assertWaits(t, h.waits, []time.Duration{30 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=503 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=503 sleep=30s attempt=0 max_attempts=15`,
	})
}

// The 5xx case never parses the body. The 429 case parses it on every attempt,
// and retryablehttp drains the restored body after the last one.
func TestRetryClient_GivesUpAfterRetryMax(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		message string
	}{
		{name: "500 without body", status: http.StatusInternalServerError},
		{name: "429 with JSON body", status: http.StatusTooManyRequests, body: `{"message":"API rate limit exceeded for user ID 1."}`, message: "API rate limit exceeded for user ID 1."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlers := make([]http.HandlerFunc, 16)
			for i := range handlers {
				handlers[i] = respond(tt.status, nil, tt.body)
			}
			srv, calls := sequenceServer(t, handlers...)
			h := newRetryHarness(t, rand.Float64)

			h.expectGiveUp(t, srv.URL+"/repos/o/r", "GET "+srv.URL+"/repos/o/r giving up after 16 attempt(s)")

			assertCalls(t, calls, 16)
			// DefaultBackoff doubles 30 s per attempt and stops at RetryWaitMax (900 s).
			wantWaits := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second}
			for len(wantWaits) < 15 {
				wantWaits = append(wantWaits, 900*time.Second)
			}
			assertWaits(t, h.waits, wantWaits)

			// CheckRetry logs every attempt. After the 16th, retryablehttp gives
			// up without calling Backoff.
			retrying := fmt.Sprintf(`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=%d message=%q`, tt.status, tt.message)
			var wantLines []string
			for i, wait := range wantWaits {
				wantLines = append(wantLines,
					retrying,
					fmt.Sprintf(`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=%d sleep=%s attempt=%d max_attempts=15`, tt.status, wait, i),
				)
			}
			wantLines = append(wantLines, retrying)
			assertLines(t, h.logLines(srv.URL), wantLines)
		})
	}
}

// CheckRetry reads the body of a 4xx response. The caller (go-github) reads it
// again to build its error, so the body must still be readable.
func TestRetryClient_NotFoundKeepsBody(t *testing.T) {
	const body = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
	srv, calls := sequenceServer(t,
		respond(http.StatusNotFound, nil, body),
	)
	h := newRetryHarness(t, rand.Float64)

	if got := h.expectResponse(t, srv.URL+"/repos/o/missing", http.StatusNotFound); got != body {
		t.Errorf("got body %q, want %q", got, body)
	}

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// NewApp puts the retry client under SearchModder, the pagination client and
// go-github. For a 404 that CheckRetry does not retry, go-github must still get
// the error message from the body that CheckRetry read before.
func TestRetryClient_GoGitHubGetsNotFoundMessage(t *testing.T) {
	const docURL = "https://docs.github.com/rest/repos/repos#get-a-repository"
	srv, calls := sequenceServer(t,
		respond(http.StatusNotFound, http.Header{"Content-Type": {"application/json"}}, `{"message":"Not Found","documentation_url":"`+docURL+`"}`),
	)
	h := newRetryHarness(t, rand.Float64)

	// The transport chain of NewApp for github.com. Instead of the GitHub URL
	// (or WithEnterpriseURLs for GitHub Enterprise), BaseURL points to the test server.
	transport := &clients.SearchModder{Base: &retryablehttp.RoundTripper{Client: h.client}}
	gh := gogithub.NewClient(githubpagination.NewClient(transport, githubpagination.WithPerPage(100))).WithAuthToken("test-token")
	baseURL, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parsing server URL: %v", err)
	}
	gh.BaseURL = baseURL

	_, _, err = gh.Repositories.Get(context.Background(), "o", "missing")
	var errResp *gogithub.ErrorResponse
	if !errors.As(err, &errResp) {
		t.Fatalf("got error %v (%T), want *github.ErrorResponse", err, err)
	}
	if errResp.Response.StatusCode != http.StatusNotFound || errResp.Message != "Not Found" || errResp.DocumentationURL != docURL {
		t.Errorf("got status %d, message %q, documentation URL %q; want 404, %q, %q",
			errResp.Response.StatusCode, errResp.Message, errResp.DocumentationURL, "Not Found", docURL)
	}

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// An empty 4xx body is no error, and the caller can still read the (empty) body.
func TestRetryClient_EmptyBodyNotFoundIsReadable(t *testing.T) {
	srv, calls := sequenceServer(t,
		respondEmptyStream(http.StatusNotFound),
	)
	h := newRetryHarness(t, rand.Float64)

	if got := h.expectResponse(t, srv.URL+"/repos/o/missing", http.StatusNotFound); got != "" {
		t.Errorf("got body %q, want empty body", got)
	}

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// A body with only a UTF-8 byte order mark is empty after the mark is removed.
// The caller can still read the (empty) body.
func TestRetryClient_ByteOrderMarkOnlyIsReadable(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusNotFound, nil, "\xef\xbb\xbf"),
	)
	h := newRetryHarness(t, rand.Float64)

	if got := h.expectResponse(t, srv.URL+"/repos/o/missing", http.StatusNotFound); got != "" {
		t.Errorf("got body %q, want empty body", got)
	}

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// When the 4xx body is no JSON, CheckRetry returns an error. retryablehttp then
// drains the body, so no caller reads it today. The test checks that CheckRetry
// does not leave a closed body behind anyway.
func TestRetryClient_CheckRetryBadJSONKeepsBody(t *testing.T) {
	const body = "<html>forbidden</html>"
	srv, _ := sequenceServer(t,
		respond(http.StatusForbidden, nil, body),
	)
	h := newRetryHarness(t, rand.Float64)

	resp, err := srv.Client().Get(srv.URL + "/repos/o/r")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	retry, err := h.client.CheckRetry(context.Background(), resp, nil)
	want := "unmarshaling response body: invalid character '<' looking for beginning of value"
	if retry || err == nil || err.Error() != want {
		t.Fatalf("got (%v, %v), want (false, %s)", retry, err, want)
	}
	if got := readBody(t, resp); got != body {
		t.Errorf("got body %q, want %q", got, body)
	}
}

// A UTF-8 byte order mark before the JSON is removed from the body.
func TestRetryClient_ByteOrderMarkIsRemoved(t *testing.T) {
	const body = `{"message":"Validation Failed"}`
	srv, calls := sequenceServer(t,
		respond(http.StatusUnprocessableEntity, nil, "\xef\xbb\xbf"+body),
	)
	h := newRetryHarness(t, rand.Float64)

	if got := h.expectResponse(t, srv.URL+"/repos/o/r/pulls", http.StatusUnprocessableEntity); got != body {
		t.Errorf("got body %q, want %q", got, body)
	}

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// The server sends fewer bytes than it declares, so reading the 4xx body fails.
func TestRetryClient_BodyReadError4xx(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"Content-Length": {"100"}}, `{"message":`),
	)
	h := newRetryHarness(t, rand.Float64)

	h.expectGiveUp(t, srv.URL+"/repos/o/r",
		"GET "+srv.URL+"/repos/o/r giving up after 1 attempt(s): parsing response body: unexpected EOF")

	assertCalls(t, calls, 1)
	assertWaits(t, h.waits, nil)
	assertLines(t, h.logLines(srv.URL), nil)
}

// Without a response, retryablehttp calls CheckRetry with the error and then
// Backoff with resp == nil. No body is involved, so the test calls both directly.
func TestRetryClient_NetworkErrors(t *testing.T) {
	h := newRetryHarness(t, rand.Float64)
	ctx := context.Background()

	// http.Client.Do wraps transport errors in a *url.Error, and retryablehttp
	// passes it to CheckRetry as it is.
	dropped := &url.Error{Op: "Get", URL: "https://api.github.com/user", Err: io.ErrUnexpectedEOF}
	if retry, err := h.client.CheckRetry(ctx, nil, dropped); !retry || err != nil {
		t.Errorf("transient error: got (%v, %v), want (true, nil)", retry, err)
	}

	permanent := errors.New("x509: certificate signed by unknown authority")
	if retry, err := h.client.CheckRetry(ctx, nil, permanent); retry || err != permanent {
		t.Errorf("permanent error: got (%v, %v), want (false, %v)", retry, err, permanent)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if retry, err := h.client.CheckRetry(cancelled, nil, dropped); retry || err != dropped {
		t.Errorf("cancelled context: got (%v, %v), want (false, %v)", retry, err, dropped)
	}

	// net/http never returns no response and no error, so this case cannot
	// happen in the client. If it did, retryablehttp would call drainBody on a
	// nil response and panic. The test only pins the current result.
	if retry, err := h.client.CheckRetry(ctx, nil, nil); !retry || err != nil {
		t.Errorf("no response and no error: got (%v, %v), want (true, nil)", retry, err)
	}

	h.client.Backoff(h.client.RetryWaitMin, h.client.RetryWaitMax, 0, nil)
	if len(h.waits) != 1 {
		t.Fatalf("got waits %v, want 1 wait", h.waits)
	}
	// DefaultBackoff gives 30 s for attempt 0, and Backoff adds up to 20 % jitter.
	if h.waits[0] < 30*time.Second || h.waits[0] >= 36*time.Second {
		t.Errorf("got wait %v, want at least 30s and less than 36s", h.waits[0])
	}

	assertLines(t, h.logLines("unused"), []string{
		`[WARN]  transient network error - will retry: error="Get \"https://api.github.com/user\": unexpected EOF"`,
		`[TRACE] waiting before retrying after network error: sleep=` + h.waits[0].String() + ` attempt=0 max_attempts=15`,
	})
}
