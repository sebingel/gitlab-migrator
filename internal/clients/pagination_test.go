package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofri/go-github-pagination/githubpagination"
	gogithub "github.com/google/go-github/v84/github"
)

// newPaginationTestServer serves /repos/o/r/pulls as two pages. The first page
// has one pull request and links to the second page. The second page answers
// with secondStatus and secondBody.
func newPaginationTestServer(t *testing.T, secondStatus int, secondBody string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/o/r/pulls" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(secondStatus)
			_, _ = fmt.Fprint(w, secondBody)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/repos/o/r/pulls?page=2&per_page=100>; rel="next"`, srv.URL))
		_, _ = fmt.Fprint(w, `[{"number":1}]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newPaginationTestGitHubClient(t *testing.T, srv *httptest.Server) *gogithub.Client {
	t.Helper()
	httpClient := NewGitHubPaginationClient(http.DefaultTransport, githubpagination.WithPerPage(100))
	gh, err := gogithub.NewClient(httpClient).WithEnterpriseURLs(srv.URL, srv.URL)
	if err != nil {
		t.Fatalf("creating GitHub client: %v", err)
	}
	return gh
}

// Each request must get its own driver: a shared driver would add the pages of
// the first request to the second one.
func TestGitHubPaginationClient_MergesPagesOfEachRequest(t *testing.T) {
	srv := newPaginationTestServer(t, http.StatusOK, `[{"number":2}]`)
	gh := newPaginationTestGitHubClient(t, srv)

	for i := range 2 {
		prs, _, err := gh.PullRequests.List(context.Background(), "o", "r", nil)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		var numbers []int
		for _, pr := range prs {
			numbers = append(numbers, pr.GetNumber())
		}
		if len(numbers) != 2 || numbers[0] != 1 || numbers[1] != 2 {
			t.Errorf("request %d: want pull requests [1 2], got %v", i+1, numbers)
		}
	}
}

func TestGitHubPaginationClient_KeepsErrorOfLaterPage(t *testing.T) {
	srv := newPaginationTestServer(t, http.StatusUnprocessableEntity,
		`{"message":"Validation Failed","documentation_url":"https://docs.github.com/rest"}`)
	gh := newPaginationTestGitHubClient(t, srv)

	_, _, err := gh.PullRequests.List(context.Background(), "o", "r", nil)

	var errResp *gogithub.ErrorResponse
	if !errors.As(err, &errResp) {
		t.Fatalf("want a *github.ErrorResponse, got %T: %v", err, err)
	}
	if errResp.Response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status: want %d, got %d", http.StatusUnprocessableEntity, errResp.Response.StatusCode)
	}
	if errResp.Message != "Validation Failed" {
		t.Errorf("message: want %q, got %q", "Validation Failed", errResp.Message)
	}
	if errResp.DocumentationURL != "https://docs.github.com/rest" {
		t.Errorf("documentation URL: want %q, got %q", "https://docs.github.com/rest", errResp.DocumentationURL)
	}
}
