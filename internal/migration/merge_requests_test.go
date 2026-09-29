package migration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "github.com/xanzy/go-gitlab"
)

// servePages answers GET requests for path with items, split into pages like
// the GitLab API does: per_page from the request (default 20, at most 100),
// and the Link and X-Next-Page headers when there is a next page. Each request
// increments calls.
func servePages[T any](t *testing.T, mux *http.ServeMux, path string, items []T, calls *atomic.Int32) {
	t.Helper()
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		query := r.URL.Query()
		perPage := 20
		if v, err := strconv.Atoi(query.Get("per_page")); err == nil && v > 0 {
			perPage = min(v, 100)
		}
		page := 1
		if v, err := strconv.Atoi(query.Get("page")); err == nil && v > 0 {
			page = v
		}
		start := min((page-1)*perPage, len(items))
		end := min(start+perPage, len(items))
		if end < len(items) {
			query.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, query.Encode()))
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(items[start:end]); err != nil {
			t.Errorf("encoding page %d of %s: %v", page, path, err)
		}
	})
}

// newGitLabTestProject returns a project for GitLab project ID 1 whose GitLab
// client talks to mux.
func newGitLabTestProject(t *testing.T, mux *http.ServeMux) *project {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gl, err := gogitlab.NewClient("test-token", gogitlab.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("creating GitLab client: %v", err)
	}

	return &project{
		m: &Migrator{
			cfg:    &config.Config{},
			gl:     gl,
			logger: hclog.NewNullLogger(),
		},
		log:        hclog.NewNullLogger(),
		project:    &gogitlab.Project{ID: 1},
		gitlabPath: []string{"group", "project"},
		githubPath: []string{"owner", "repo"},
	}
}

func TestListMergeRequestCommits_ReadsAllPages(t *testing.T) {
	// GitLab lists the newest commit first, so the oldest commits of a large
	// merge request are only on the last page.
	const total = 130
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	commits := make([]*gogitlab.Commit, 0, total)
	for i := total; i >= 1; i-- {
		committed := base.Add(time.Duration(i) * time.Minute)
		commits = append(commits, &gogitlab.Commit{ID: fmt.Sprintf("commit-%03d", i), CommittedDate: &committed})
	}

	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/7/commits", commits, &calls)
	p := newGitLabTestProject(t, mux)

	got, err := p.listMergeRequestCommits(7)
	if err != nil {
		t.Fatalf("listMergeRequestCommits: %v", err)
	}
	if len(got) != total {
		t.Fatalf("got %d commits, want %d", len(got), total)
	}
	if got[0].ID != "commit-001" {
		t.Errorf("first commit = %s, want commit-001", got[0].ID)
	}
	if last := got[len(got)-1].ID; last != "commit-130" {
		t.Errorf("last commit = %s, want commit-130", last)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("GitLab requests = %d, want 2 (100 commits per page)", n)
	}
}
