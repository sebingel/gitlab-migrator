package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
)

const secondaryRateLimitBody = `{"message":"You have exceeded a secondary rate limit and have been temporarily blocked from content creation.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`

// retryHarness runs requests through the retry client from buildRetryClient,
// so CheckRetry and Backoff run in the order that retryablehttp uses.
type retryHarness struct {
	client *retryablehttp.Client
	logs   *bytes.Buffer
	// waits holds every wait that Backoff computed, in call order.
	waits []time.Duration
}

// newRetryHarness wraps Backoff: the wrapper records the computed wait and
// returns 0, so the tests do not sleep. The Backoff log lines still show the
// computed wait.
func newRetryHarness(t *testing.T) *retryHarness {
	t.Helper()

	h := &retryHarness{logs: &bytes.Buffer{}}
	logger := hclog.New(&hclog.LoggerOptions{
		Level:       hclog.Trace,
		Output:      h.logs,
		DisableTime: true,
	})
	h.client = buildRetryClient(logger)

	backoff := h.client.Backoff
	h.client.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
		h.waits = append(h.waits, backoff(min, max, attemptNum, resp))
		return 0
	}

	return h
}

// logLines returns the captured log output with the test server URL replaced by "SERVER".
func (h *retryHarness) logLines(serverURL string) []string {
	out := strings.ReplaceAll(h.logs.String(), serverURL, "SERVER")
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
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
	if len(got) != len(want) {
		t.Fatalf("got waits %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wait %d: got %v, want %v", i, got[i], want[i])
		}
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

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return string(b)
}

// retryablehttp drains the response body before it calls Backoff. So Backoff
// cannot read the secondary rate limit message, and it uses the default backoff.
func TestRetryClient_SecondaryRateLimit403(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, secondaryRateLimitBody),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/r/pulls")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "ok" {
		t.Fatalf("got status %d, want 200 with body ok", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d requests, want 2", got)
	}

	assertWaits(t, h.waits, []time.Duration{30 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[WARN]  secondary rate limit exceeded - will retry with extended backoff: message="You have exceeded a secondary rate limit and have been temporarily blocked from content creation." method=GET url=SERVER/repos/o/r/pulls`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r/pulls status=403 sleep=30s attempt=0 max_attempts=15`,
	})
}

func TestRetryClient_SAMLEnforcement403(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, `{"message":"Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization.","documentation_url":"https://docs.github.com/articles/authenticating-to-a-github-organization-with-saml-single-sign-on/"}`),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/orgs/o/repos")
	if resp != nil {
		t.Errorf("got response with status %d, want nil", resp.StatusCode)
	}
	want := "GET " + srv.URL + "/orgs/o/repos giving up after 1 attempt(s): received 403 with response: Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization. - https://docs.github.com/articles/authenticating-to-a-github-organization-with-saml-single-sign-on/"
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v\nwant %s", err, want)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d requests, want 1", got)
	}
	assertWaits(t, h.waits, nil)
	if h.logs.Len() != 0 {
		t.Errorf("got log output %q, want none", h.logs.String())
	}
}

func TestRetryClient_TooManyRequestsWithRetryAfter(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, `{"message":"API rate limit exceeded for user ID 1."}`),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "ok" {
		t.Fatalf("got status %d, want 200 with body ok", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d requests, want 2", got)
	}

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
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/r")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "ok" {
		t.Fatalf("got status %d, want 200 with body ok", resp.StatusCode)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("got %d requests, want 4", got)
	}

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

// A 4xx body that is no JSON stops the request, even for a retryable status.
func TestRetryClient_BadJSON4xx(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, "<html>forbidden</html>"),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/r")
	if resp != nil {
		t.Errorf("got response with status %d, want nil", resp.StatusCode)
	}
	want := "GET " + srv.URL + "/repos/o/r giving up after 1 attempt(s): unmarshaling response body: invalid character '<' looking for beginning of value"
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v\nwant %s", err, want)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d requests, want 1", got)
	}
	assertWaits(t, h.waits, nil)
	if h.logs.Len() != 0 {
		t.Errorf("got log output %q, want none", h.logs.String())
	}
}

func TestRetryClient_EmptyBody403(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, nil, ""),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/r")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "ok" {
		t.Fatalf("got status %d, want 200 with body ok", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("got %d requests, want 2", got)
	}

	assertWaits(t, h.waits, []time.Duration{30 * time.Second})
	assertLines(t, h.logLines(srv.URL), []string{
		`[TRACE] retrying failed API request: method=GET url=SERVER/repos/o/r status=403 message=""`,
		`[TRACE] waiting before retrying failed API request: method=GET url=SERVER/repos/o/r status=403 sleep=30s attempt=0 max_attempts=15`,
	})
}

func TestRetryClient_PrimaryRateLimitHeaders(t *testing.T) {
	const body = `{"message":"API rate limit exceeded for user ID 1."}`
	reset := strconv.FormatInt(time.Now().Unix()+100, 10)
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, body),
		respond(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, body),
		respond(http.StatusOK, nil, "ok"),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "ok" {
		t.Fatalf("got status %d, want 200 with body ok", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("got %d requests, want 3", got)
	}

	if len(h.waits) != 2 {
		t.Fatalf("got waits %v, want 2 waits", h.waits)
	}
	// The reset time has whole seconds, and Backoff adds 30 s and rounds to seconds.
	if h.waits[0] < 129*time.Second || h.waits[0] > 130*time.Second {
		t.Errorf("wait 0: got %v, want 2m9s or 2m10s", h.waits[0])
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

// CheckRetry reads the body of a 4xx response. The caller (go-github) reads it
// again to build its error, so the body must still be readable.
func TestRetryClient_NotFoundKeepsBody(t *testing.T) {
	const body = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
	srv, calls := sequenceServer(t,
		respond(http.StatusNotFound, nil, body),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", resp.StatusCode)
	}
	if got := readBody(t, resp); got != body {
		t.Errorf("got body %q, want %q", got, body)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d requests, want 1", got)
	}
	assertWaits(t, h.waits, nil)
	if h.logs.Len() != 0 {
		t.Errorf("got log output %q, want none", h.logs.String())
	}
}

// A UTF-8 byte order mark before the JSON is removed from the body.
func TestRetryClient_ByteOrderMarkIsRemoved(t *testing.T) {
	const body = `{"message":"Validation Failed"}`
	srv, _ := sequenceServer(t,
		respond(http.StatusUnprocessableEntity, nil, "\xef\xbb\xbf"+body),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/r/pulls")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want 422", resp.StatusCode)
	}
	if got := readBody(t, resp); got != body {
		t.Errorf("got body %q, want %q", got, body)
	}
}

// The server sends fewer bytes than it declares, so reading the 4xx body fails.
func TestRetryClient_BodyReadError4xx(t *testing.T) {
	srv, calls := sequenceServer(t,
		respond(http.StatusForbidden, http.Header{"Content-Length": {"100"}}, `{"message":`),
	)
	h := newRetryHarness(t)

	resp, err := h.client.Get(srv.URL + "/repos/o/r")
	if resp != nil {
		t.Errorf("got response with status %d, want nil", resp.StatusCode)
	}
	want := "GET " + srv.URL + "/repos/o/r giving up after 1 attempt(s): parsing response body: unexpected EOF"
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v\nwant %s", err, want)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d requests, want 1", got)
	}
	assertWaits(t, h.waits, nil)
	if h.logs.Len() != 0 {
		t.Errorf("got log output %q, want none", h.logs.String())
	}
}

// Without a response, retryablehttp calls CheckRetry with the error and then
// Backoff with resp == nil. No body is involved, so the test calls both directly.
func TestRetryClient_NetworkErrors(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()

	if retry, err := h.client.CheckRetry(ctx, nil, io.ErrUnexpectedEOF); !retry || err != nil {
		t.Errorf("transient error: got (%v, %v), want (true, nil)", retry, err)
	}

	permanent := errors.New("x509: certificate signed by unknown authority")
	if retry, err := h.client.CheckRetry(ctx, nil, permanent); retry || err != permanent {
		t.Errorf("permanent error: got (%v, %v), want (false, %v)", retry, err, permanent)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if retry, err := h.client.CheckRetry(cancelled, nil, io.ErrUnexpectedEOF); retry || err != io.ErrUnexpectedEOF {
		t.Errorf("cancelled context: got (%v, %v), want (false, %v)", retry, err, io.ErrUnexpectedEOF)
	}

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
		`[WARN]  transient network error - will retry: error="unexpected EOF"`,
		`[TRACE] waiting before retrying after network error: sleep=` + h.waits[0].String() + ` attempt=0 max_attempts=15`,
	})
}
