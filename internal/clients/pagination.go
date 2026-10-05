package clients

import (
	"net/http"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/gofri/go-github-pagination/githubpagination/drivers"
)

// NewGitHubPaginationClient returns an *http.Client like
// githubpagination.NewClient, but an error response keeps its own body.
//
// githubpagination v1.0.1 stops at the first page that has no 200 status and
// returns that response. When earlier pages were read, its default driver then
// replaces the body of the error response with the merged earlier pages, so
// go-github cannot read the message of the error. Here every request gets its
// own keepErrorBodyDriver, which leaves the body of an error response alone.
func NewGitHubPaginationClient(base http.RoundTripper, opts ...githubpagination.Option) *http.Client {
	return &http.Client{
		Transport: &keepErrorBodyTransport{next: githubpagination.New(base, opts...)},
	}
}

// keepErrorBodyTransport gives every request a new keepErrorBodyDriver through
// the config override in its context. A driver holds the pages of one request,
// so requests cannot share one. An override that the caller put in the context
// is applied after this one, so a driver of the caller still wins.
type keepErrorBodyTransport struct {
	next http.RoundTripper
}

func (t *keepErrorBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	driver := &keepErrorBodyDriver{Driver: drivers.NewSyncPaginationDriver()}
	ctx := githubpagination.WithOverrideConfig(req.Context(), githubpagination.WithDriver(driver))
	return t.next.RoundTrip(req.WithContext(ctx))
}

// keepErrorBodyDriver is the default sync driver of githubpagination, but it
// does not merge into the response that OnBadResponse got. githubpagination
// calls OnBadResponse for a response without a 200 status, stops, and then
// calls OnFinish with that response.
type keepErrorBodyDriver struct {
	drivers.Driver
	badResponse *http.Response
}

func (d *keepErrorBodyDriver) OnBadResponse(resp *http.Response, err error) {
	d.badResponse = resp
	d.Driver.OnBadResponse(resp, err)
}

func (d *keepErrorBodyDriver) OnFinish(resp *http.Response, pageCount int) error {
	if resp != nil && resp == d.badResponse {
		return nil
	}
	return d.Driver.OnFinish(resp, pageCount)
}
