package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
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

	got, err := p.listMergeRequestCommits(context.Background(), 7)
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

func TestListMergeRequestCommits_UsesGitLabOrder(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		// dates[i] is the committed date of commit-(i+1) after base. The
		// commits are in git order, oldest first.
		dates []time.Duration
	}{
		// A rebase can give all commits the same committed date, because the
		// date has only one second resolution.
		{"same date, one page", make([]time.Duration, 3)},
		{"same date, two pages", make([]time.Duration, 130)},
		// A wrong clock can give an older commit a later date.
		{"first commit has the latest date", []time.Duration{time.Hour, 0, time.Minute}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total := len(tt.dates)
			commits := make([]*gogitlab.Commit, 0, total)
			// GitLab lists the newest commit first.
			for i := total; i >= 1; i-- {
				committed := base.Add(tt.dates[i-1])
				commits = append(commits, &gogitlab.Commit{ID: fmt.Sprintf("commit-%03d", i), CommittedDate: &committed})
			}

			mux := http.NewServeMux()
			var calls atomic.Int32
			servePages(t, mux, "/api/v4/projects/1/merge_requests/7/commits", commits, &calls)
			p := newGitLabTestProject(t, mux)

			got, err := p.listMergeRequestCommits(context.Background(), 7)
			if err != nil {
				t.Fatalf("listMergeRequestCommits: %v", err)
			}
			if len(got) != total {
				t.Fatalf("got %d commits, want %d", len(got), total)
			}
			for i, c := range got {
				if want := fmt.Sprintf("commit-%03d", i+1); c.ID != want {
					t.Fatalf("commit %d = %s, want %s (oldest first)", i, c.ID, want)
				}
			}
		})
	}
}

// serveUntilCanceled answers GET requests for path only when the request
// context ends or the test is over. It tells started when a request arrived.
// Call it after the test server was started: the cleanup that ends waiting
// requests must run before the server closes, because Close waits for them.
func serveUntilCanceled(t *testing.T, mux *http.ServeMux, path string, started chan<- struct{}) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
}

// wantCanceledAfterStart cancels the context once the first request arrived
// and checks that call returns context.Canceled soon after.
func wantCanceledAfterStart(t *testing.T, started <-chan struct{}, cancel context.CancelFunc, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no GitLab request arrived")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the GitLab request did not stop after the context was canceled")
	}
}

func TestListMergeRequestCommits_StopsWhenContextIsCanceled(t *testing.T) {
	mux := http.NewServeMux()
	p := newGitLabTestProject(t, mux)
	started := make(chan struct{}, 1)
	serveUntilCanceled(t, mux, "/api/v4/projects/1/merge_requests/7/commits", started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantCanceledAfterStart(t, started, cancel, func() error {
		_, err := p.listMergeRequestCommits(ctx, 7)
		return err
	})
}

func TestNewProject_StopsWhenContextIsCanceled(t *testing.T) {
	mux := http.NewServeMux()
	p := newGitLabTestProject(t, mux)
	started := make(chan struct{}, 1)
	serveUntilCanceled(t, mux, "/api/v4/projects/{id}", started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantCanceledAfterStart(t, started, cancel, func() error {
		_, err := p.m.newProject(ctx, []string{"group/project", "owner/repo"})
		return err
	})
}

func TestMigrateMergeRequests_StopsWhenContextIsCanceled(t *testing.T) {
	mux := http.NewServeMux()
	p := newGitLabTestProject(t, mux)
	started := make(chan struct{}, 1)
	serveUntilCanceled(t, mux, "/api/v4/projects/1/merge_requests", started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantCanceledAfterStart(t, started, cancel, func() error {
		results, err := p.migrateMergeRequests(ctx)
		if len(results) != 0 {
			return fmt.Errorf("results = %+v, want none", results)
		}
		return err
	})
}

func TestMigrate_FailsWhenMergeRequestListFails(t *testing.T) {
	// The project must not be reported as migrated when its merge requests
	// could not be listed, because then none of them were migrated.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/1/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
	})
	p := newGitLabTestProject(t, mux)
	p.m.cfg.EnablePullRequests = true
	p.m.cfg.PullRequestsOnly = true
	serveGitHubRepo(t, p)

	result, err := p.migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "retrieving gitlab merge requests") {
		t.Fatalf("migrate error = %v, want the failed merge request list", err)
	}
	wantFinishedResult(t, result)
}

// serveGitHubRepo points the GitHub client of p to a test server that knows
// the repository owner/repo, so migrate() can run in pull-requests-only mode.
func serveGitHubRepo(t *testing.T, p *project) {
	t.Helper()
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("GET /repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"name":"repo"}`); err != nil {
			t.Errorf("writing the GitHub repository: %v", err)
		}
	})
	ghSrv := httptest.NewServer(ghMux)
	t.Cleanup(ghSrv.Close)
	gh := gogithub.NewClient(nil)
	baseURL, err := url.Parse(ghSrv.URL + "/")
	if err != nil {
		t.Fatalf("parsing GitHub test server URL: %v", err)
	}
	gh.BaseURL = baseURL
	p.m.gh = gh
}

// cancelOnLog is a log output that calls cancel when a log line contains msg.
type cancelOnLog struct {
	msg    string
	cancel context.CancelFunc
}

func (c cancelOnLog) Write(b []byte) (int, error) {
	if strings.Contains(string(b), c.msg) {
		c.cancel()
	}
	return len(b), nil
}

func TestMigrate_FailsWhenInterruptedBeforeAllMergeRequests(t *testing.T) {
	// The project must not be reported as migrated when the run is canceled
	// after the merge requests were listed but before all were processed.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/1/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[{"iid":1,"state":"merged"},{"iid":2,"state":"merged"}]`); err != nil {
			t.Errorf("writing the merge requests: %v", err)
		}
	})
	p := newGitLabTestProject(t, mux)
	p.m.cfg.EnablePullRequests = true
	p.m.cfg.PullRequestsOnly = true
	serveGitHubRepo(t, p)

	// The cancel comes with the log line that is written after the list and
	// before the loop over the merge requests, so no request is in flight.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.log = hclog.New(&hclog.LoggerOptions{
		Level:  hclog.Info,
		Output: cancelOnLog{msg: "migrating merge requests from GitLab to GitHub", cancel: cancel},
	})

	result, err := p.migrate(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("migrate error = %v, want context.Canceled", err)
	}
	wantFinishedResult(t, result)
}

func TestMigrate_SetsEndTimeWhenStateDirFails(t *testing.T) {
	// Every early return of migrate() must leave a finished result, not only
	// the merge request error path.
	mux := http.NewServeMux()
	p := newGitLabTestProject(t, mux)
	p.m.cfg.EnablePullRequests = true
	p.m.cfg.PullRequestsOnly = true
	serveGitHubRepo(t, p)

	// A state dir below a regular file cannot be created.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("writing the blocking file: %v", err)
	}
	p.m.cfg.StateDir = filepath.Join(file, "state")

	result, err := p.migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "creating state directory") {
		t.Fatalf("migrate error = %v, want the failed state directory", err)
	}
	wantFinishedResult(t, result)
}

// wantFinishedResult checks that the report fields that migrate() sets at its
// end are set, also for a project that failed.
func wantFinishedResult(t *testing.T, result ProjectResult) {
	t.Helper()
	if result.EndTime.IsZero() {
		t.Error("EndTime is not set")
	}
	if result.Duration != result.EndTime.Sub(result.StartTime) {
		t.Errorf("Duration = %v, want EndTime - StartTime = %v", result.Duration, result.EndTime.Sub(result.StartTime))
	}
	if result.BranchCount != len(result.BranchesMigrated) {
		t.Errorf("BranchCount = %d, want %d", result.BranchCount, len(result.BranchesMigrated))
	}
}

func TestReportProject_StopsWhenContextIsCanceled(t *testing.T) {
	slugs := []string{"group/project", "owner/repo"}

	t.Run("retrieving the project", func(t *testing.T) {
		mux := http.NewServeMux()
		p := newGitLabTestProject(t, mux)
		started := make(chan struct{}, 1)
		serveUntilCanceled(t, mux, "/api/v4/projects/{id}", started)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wantCanceledAfterStart(t, started, cancel, func() error {
			_, err := p.m.reportProject(ctx, slugs)
			return err
		})
	})

	t.Run("listing merge requests", func(t *testing.T) {
		mux := http.NewServeMux()
		p := newGitLabTestProject(t, mux)
		serveProjects(t, mux, []*gogitlab.Project{{ID: 1, PathWithNamespace: "group/project"}})
		started := make(chan struct{}, 1)
		serveUntilCanceled(t, mux, "/api/v4/projects/1/merge_requests", started)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wantCanceledAfterStart(t, started, cancel, func() error {
			_, err := p.m.reportProject(ctx, slugs)
			return err
		})
	})
}

// serveProjects answers the lookup of one project by its path or its ID with
// the matching project of projects. It serves no project search: a report
// that searches gets a 404 and fails.
func serveProjects(t *testing.T, mux *http.ServeMux, projects []*gogitlab.Project) {
	t.Helper()
	mux.HandleFunc("GET /api/v4/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, proj := range projects {
			if proj.PathWithNamespace == r.PathValue("id") || strconv.FormatInt(proj.ID, 10) == r.PathValue("id") {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(proj); err != nil {
					t.Errorf("encoding project %s: %v", proj.PathWithNamespace, err)
				}
				return
			}
		}
		http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
	})
}

func TestReportProject_FindsProjectAfterFirstPage(t *testing.T) {
	// Many projects of other groups are also named "project". GitLab lists 20
	// per page by default, so a search for the name would list group/project
	// only on page 2. The report looks the project up by its path and must
	// find it anyway.
	projects := make([]*gogitlab.Project, 0, 30)
	for i := 1; i <= 25; i++ {
		projects = append(projects, &gogitlab.Project{ID: int64(100 + i), PathWithNamespace: fmt.Sprintf("other-%02d/project", i)})
	}
	projects = append(projects, &gogitlab.Project{ID: 1, PathWithNamespace: "group/project"})

	mux := http.NewServeMux()
	serveProjects(t, mux, projects)
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests", []*gogitlab.BasicMergeRequest{{IID: 1, State: "merged"}, {IID: 2, State: "closed"}}, &calls)
	p := newGitLabTestProject(t, mux)

	report, err := p.m.reportProject(context.Background(), []string{"group/project", "owner/repo"})
	if err != nil {
		t.Fatalf("reportProject: %v", err)
	}
	if report.MergeRequestsCount != 2 {
		t.Errorf("merge requests = %d, want 2", report.MergeRequestsCount)
	}
}

// serveMergeRequestsCreatedAfter answers the merge request list of project 1
// with the merge requests that were created after the created_after parameter
// of the request, like GitLab does. Without the parameter it answers all.
func serveMergeRequestsCreatedAfter(t *testing.T, mux *http.ServeMux, mergeRequests []*gogitlab.BasicMergeRequest) {
	t.Helper()
	mux.HandleFunc("GET /api/v4/projects/1/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		var after time.Time
		if v := r.URL.Query().Get("created_after"); v != "" {
			var err error
			if after, err = time.Parse(time.RFC3339, v); err != nil {
				t.Errorf("parsing created_after %q: %v", v, err)
			}
		}
		matching := make([]*gogitlab.BasicMergeRequest, 0, len(mergeRequests))
		for _, mr := range mergeRequests {
			if mr.CreatedAt.After(after) {
				matching = append(matching, mr)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(matching); err != nil {
			t.Errorf("encoding merge requests: %v", err)
		}
	})
}

func TestReportProject_CountsOnlyMergeRequestsInMaxAge(t *testing.T) {
	// With -merge-requests-max-age 30 a migration processes only the merge
	// request of 10 days ago, so the report must count only that one.
	now := time.Now()
	mergeRequests := []*gogitlab.BasicMergeRequest{
		{IID: 1, State: "merged", CreatedAt: Pointer(now.AddDate(0, 0, -100))},
		{IID: 2, State: "merged", CreatedAt: Pointer(now.AddDate(0, 0, -10))},
	}

	mux := http.NewServeMux()
	serveProjects(t, mux, []*gogitlab.Project{{ID: 1, PathWithNamespace: "group/project"}})
	serveMergeRequestsCreatedAfter(t, mux, mergeRequests)
	p := newGitLabTestProject(t, mux)
	p.m.cfg.MergeRequestsAge = 30

	report, err := p.m.reportProject(context.Background(), []string{"group/project", "owner/repo"})
	if err != nil {
		t.Fatalf("reportProject: %v", err)
	}
	if report.MergeRequestsCount != 1 {
		t.Errorf("merge requests = %d, want 1 (only the one of the last 30 days)", report.MergeRequestsCount)
	}
}

func TestListMergeRequestNotes_StopsWhenContextIsCanceled(t *testing.T) {
	mux := http.NewServeMux()
	p := newGitLabTestProject(t, mux)
	started := make(chan struct{}, 1)
	serveUntilCanceled(t, mux, "/api/v4/projects/1/merge_requests/7/notes", started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantCanceledAfterStart(t, started, cancel, func() error {
		_, err := p.listMergeRequestNotes(ctx, 7)
		return err
	})
}

func TestListMergeRequestAwardEmoji_StopsWhenContextIsCanceled(t *testing.T) {
	mux := http.NewServeMux()
	p := newGitLabTestProject(t, mux)
	started := make(chan struct{}, 1)
	serveUntilCanceled(t, mux, "/api/v4/projects/1/merge_requests/7/award_emoji", started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantCanceledAfterStart(t, started, cancel, func() error {
		_, err := p.listMergeRequestAwardEmoji(ctx, 7)
		return err
	})
}

func TestListMergeRequestAwardEmoji_ReadsAllPages(t *testing.T) {
	// A thumbs up after the first 100 award emoji is on the second page.
	const total = 130
	awards := make([]*gogitlab.AwardEmoji, total)
	for i := range awards {
		awards[i] = &gogitlab.AwardEmoji{ID: int64(i + 1), Name: "rocket"}
	}
	awards[119].Name = "thumbsup"
	awards[119].User.Username = "late-approver"

	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/7/award_emoji", awards, &calls)
	p := newGitLabTestProject(t, mux)

	got, err := p.listMergeRequestAwardEmoji(context.Background(), 7)
	if err != nil {
		t.Fatalf("listMergeRequestAwardEmoji: %v", err)
	}
	if len(got) != total {
		t.Fatalf("got %d award emoji, want %d", len(got), total)
	}
	if got[119].Name != "thumbsup" || got[119].User.Username != "late-approver" {
		t.Errorf("award 120 = %s by %q, want thumbsup by late-approver", got[119].Name, got[119].User.Username)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("GitLab requests = %d, want 2 (100 award emoji per page)", n)
	}
}

// failingSearchGitHub is a GitHubClient whose search always fails. A merge
// request that reaches migrateMergeRequest ends as failed right after the
// search, without any other GitHub call.
type failingSearchGitHub struct {
	searches int
}

func (f *failingSearchGitHub) GetBranches(context.Context, string, string) ([]*gogithub.Branch, error) {
	return nil, errors.New("GetBranches is not expected in this test")
}

func (f *failingSearchGitHub) GetPullRequest(context.Context, string, string, int) (*gogithub.PullRequest, error) {
	return nil, errors.New("GetPullRequest is not expected in this test")
}

func (f *failingSearchGitHub) GetSearchResults(context.Context, string) (*gogithub.IssuesSearchResult, error) {
	f.searches++
	return nil, errors.New("search failed in test")
}

// cancelingSearchGitHub is a failingSearchGitHub that calls cancel in its
// search, like a Ctrl+C while the merge request is processed.
type cancelingSearchGitHub struct {
	failingSearchGitHub
	cancel context.CancelFunc
}

func (c *cancelingSearchGitHub) GetSearchResults(ctx context.Context, query string) (*gogithub.IssuesSearchResult, error) {
	c.cancel()
	return c.failingSearchGitHub.GetSearchResults(ctx, query)
}

func TestMigrateMergeRequests_FailsWhenInterruptedDuringLastMergeRequest(t *testing.T) {
	// A cancel while the last merge request is processed must stop the
	// project too, whatever status that merge request ends with. In real
	// runs it can end as partial (a canceled GitLab user lookup only fails a
	// comment), which counts as migrated. Here the fake search fails, so MR
	// !3 ends as failed. The check after the loop does not look at the
	// status, so a failed merge request is enough to test it.
	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests", []*gogitlab.BasicMergeRequest{
		{IID: 3, Title: "some work", State: "merged", SourceBranch: "feature", TargetBranch: "main"},
	}, &calls)
	p := newGitLabTestProject(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh := &cancelingSearchGitHub{cancel: cancel}
	p.m.ghClient = gh

	results, err := p.migrateMergeRequests(ctx)
	if gh.searches != 1 {
		t.Fatalf("searches = %d, want 1 (MR !3 processed)", gh.searches)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want the processed MR !3", results)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestMigrateMergeRequests_OpenMergeRequestSkipIsNotFinal(t *testing.T) {
	// migrate runs migrateMergeRequests like a new process of the tool: it
	// loads the state file and migrates MR !3 in the given GitLab state. It
	// returns the results and the number of GitHub searches, which is 1 when
	// the merge request was really processed.
	migrate := func(t *testing.T, statePath, mrState string, skipOpen bool) ([]MergeRequestResult, int) {
		t.Helper()
		mux := http.NewServeMux()
		var calls atomic.Int32
		servePages(t, mux, "/api/v4/projects/1/merge_requests", []*gogitlab.BasicMergeRequest{
			{IID: 3, Title: "some work", State: mrState, SourceBranch: "feature", TargetBranch: "main"},
		}, &calls)
		p := newGitLabTestProject(t, mux)
		p.m.cfg.SkipOpenMergeRequests = skipOpen
		gh := &failingSearchGitHub{}
		p.m.ghClient = gh

		state, err := LoadOrCreate(statePath, "group/project", "owner/repo", testLogger())
		if err != nil {
			t.Fatalf("LoadOrCreate: %v", err)
		}
		p.state = state

		results, err := p.migrateMergeRequests(context.Background())
		if err != nil {
			t.Fatalf("migrateMergeRequests: %v", err)
		}
		return results, gh.searches
	}

	// wantProcessed checks that MR !3 went through migrateMergeRequest. The
	// fake search makes the migration fail.
	wantProcessed := func(t *testing.T, results []MergeRequestResult, searches int) {
		t.Helper()
		if len(results) != 1 || results[0].Status != StatusFailed || searches != 1 {
			t.Fatalf("results = %+v, searches = %d, want MR !3 processed (1 search, failed at the fake search)", results, searches)
		}
	}

	runWithFlag := func(t *testing.T, statePath string) {
		t.Helper()
		results, searches := migrate(t, statePath, "opened", true)
		if len(results) != 1 || results[0].Status != StatusSkipped || results[0].SkipReason != skipReasonOpenMergeRequest || searches != 0 {
			t.Fatalf("run with -skip-open-merge-requests: results = %+v, searches = %d, want MR !3 skipped without a search", results, searches)
		}

		saved, err := LoadOrCreate(statePath, "group/project", "owner/repo", testLogger())
		if err != nil {
			t.Fatalf("reloading state: %v", err)
		}
		if st := saved.GetState(3); st != nil {
			t.Fatalf("state for MR !3 = %+v, want no entry: the skip depends on the flags of this run", *st)
		}
	}

	t.Run("next run without the flag", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "state.json")
		runWithFlag(t, statePath)

		results, searches := migrate(t, statePath, "opened", false)
		wantProcessed(t, results, searches)
	})

	t.Run("merged before the next run with the flag", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "state.json")
		runWithFlag(t, statePath)

		results, searches := migrate(t, statePath, "merged", true)
		wantProcessed(t, results, searches)
	})

	// writeOldStateFile writes a state file as older versions wrote it after a
	// run with -skip-open-merge-requests.
	writeOldStateFile := func(t *testing.T) string {
		t.Helper()
		statePath := filepath.Join(t.TempDir(), "state.json")
		old := `{"version":1,"gitlab_project":"group/project","github_repo":"owner/repo","updated_at":"2026-01-01T00:00:00Z",` +
			`"merge_requests":{"3":{"status":"skipped","skip_reason":"open merge request skipped (-skip-open-merge-requests)","updated_at":"2026-01-01T00:00:00Z"}}}`
		if err := os.WriteFile(statePath, []byte(old), 0644); err != nil {
			t.Fatal(err)
		}
		return statePath
	}

	t.Run("state file of an older version, next run without the flag", func(t *testing.T) {
		results, searches := migrate(t, writeOldStateFile(t), "opened", false)
		wantProcessed(t, results, searches)
	})

	t.Run("state file of an older version, merged before the next run with the flag", func(t *testing.T) {
		results, searches := migrate(t, writeOldStateFile(t), "merged", true)
		wantProcessed(t, results, searches)
	})
}

// searchGitHub is a GitHubClient whose search returns issues, whose
// GetPullRequest returns the pull requests in prs and whose GetBranches
// returns branches. It records the numbers that GetPullRequest was called with.
type searchGitHub struct {
	issues    []*gogithub.Issue
	prs       map[int]*gogithub.PullRequest
	branches  []*gogithub.Branch
	requested []int
}

func (f *searchGitHub) GetBranches(context.Context, string, string) ([]*gogithub.Branch, error) {
	if f.branches == nil {
		return nil, errors.New("GetBranches is not expected in this test")
	}
	return f.branches, nil
}

func (f *searchGitHub) GetPullRequest(_ context.Context, _, _ string, number int) (*gogithub.PullRequest, error) {
	f.requested = append(f.requested, number)
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request %d is not expected in this test", number)
	}
	return pr, nil
}

func (f *searchGitHub) GetSearchResults(context.Context, string) (*gogithub.IssuesSearchResult, error) {
	return &gogithub.IssuesSearchResult{Issues: f.issues}, nil
}

func TestMigrateMergeRequest_SearchResultWithoutPullRequestURL(t *testing.T) {
	// The search finds pull request 5, but GitHub sent no URL for it. Its
	// number is the number of the issue. Its body belongs to another merge
	// request, so MR !3 goes on and is skipped because it has no commits.
	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/commits", []*gogitlab.Commit{}, &calls)
	p := newGitLabTestProject(t, mux)
	gh := &searchGitHub{
		issues: []*gogithub.Issue{{Number: Pointer(5), PullRequestLinks: &gogithub.PullRequestLinks{}}},
		prs:    map[int]*gogithub.PullRequest{5: {Number: Pointer(5), Body: Pointer("> | **GitLab MR Number** | 4 |")}},
	}
	p.m.ghClient = gh

	result, err := p.migrateMergeRequest(context.Background(), &gogitlab.BasicMergeRequest{
		IID: 3, Title: "some work", State: "merged", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatalf("migrateMergeRequest: %v", err)
	}
	if len(gh.requested) != 1 || gh.requested[0] != 5 {
		t.Errorf("requested pull requests = %v, want [5]", gh.requested)
	}
	if result.Status != StatusSkipped || result.SkipReason != "merge request has no commits" {
		t.Errorf("result = %+v, want skipped because the merge request has no commits", result)
	}
}

func TestMigrateMergeRequest_MergeRequestWithoutCreationDate(t *testing.T) {
	// MR !3 has no creation date and its pull request 5 exists with an older
	// body, so the body is written again.
	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/award_emoji", []*gogitlab.AwardEmoji{}, &calls)
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/notes", []*gogitlab.Note{}, &calls)
	p := newGitLabTestProject(t, mux)
	p.m.ghClient = &searchGitHub{
		issues: []*gogithub.Issue{{
			Number:           Pointer(5),
			PullRequestLinks: &gogithub.PullRequestLinks{URL: Pointer("https://api.github.com/repos/owner/repo/pulls/5")},
		}},
		prs: map[int]*gogithub.PullRequest{5: {
			Number: Pointer(5),
			State:  Pointer("closed"),
			Title:  Pointer("some work"),
			Body:   Pointer("> | **GitLab MR Number** | 3 |"),
			Draft:  Pointer(false),
		}},
	}

	var editedBody string
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("PATCH /repos/owner/repo/pulls/5", func(w http.ResponseWriter, r *http.Request) {
		var pr gogithub.PullRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			t.Errorf("decoding the edited pull request: %v", err)
		}
		editedBody = pr.GetBody()
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"number":5,"state":"closed"}`); err != nil {
			t.Errorf("writing the edited pull request: %v", err)
		}
	})
	ghMux.HandleFunc("GET /repos/owner/repo/issues/5/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("writing the pull request comments: %v", err)
		}
	})
	ghSrv := httptest.NewServer(ghMux)
	t.Cleanup(ghSrv.Close)
	gh := gogithub.NewClient(nil)
	baseURL, err := url.Parse(ghSrv.URL + "/")
	if err != nil {
		t.Fatalf("parsing GitHub test server URL: %v", err)
	}
	gh.BaseURL = baseURL
	p.m.gh = gh

	result, err := p.migrateMergeRequest(context.Background(), &gogitlab.BasicMergeRequest{
		IID: 3, Title: "some work", State: "merged", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatalf("migrateMergeRequest: %v", err)
	}
	if result.Status != StatusSuccess {
		t.Errorf("status = %q, want %q", result.Status, StatusSuccess)
	}
	if want := "> | **Date Originally Opened** | " + unknownDate + " |"; !strings.Contains(editedBody, want) {
		t.Errorf("pull request body = %q, want it to contain %q", editedBody, want)
	}
}

// useGitHubMux points the GitHub API client of p to a test server for ghMux.
func useGitHubMux(t *testing.T, p *project, ghMux *http.ServeMux) {
	t.Helper()
	ghSrv := httptest.NewServer(ghMux)
	t.Cleanup(ghSrv.Close)
	gh := gogithub.NewClient(nil)
	baseURL, err := url.Parse(ghSrv.URL + "/")
	if err != nil {
		t.Fatalf("parsing GitHub test server URL: %v", err)
	}
	gh.BaseURL = baseURL
	p.m.gh = gh
}

// noCommitsBetween returns a handler that answers the creation of a pull
// request with the 422 error that GitHub sends when the head branch has no
// commits that the base branch does not have. It calls onCreate first, when it
// is not nil.
func noCommitsBetween(t *testing.T, onCreate func()) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if onCreate != nil {
			onCreate()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		if _, err := fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"No commits between migration-target-3/main and migration-source-3/feature"}]}`); err != nil {
			t.Errorf("writing the pull request error: %v", err)
		}
	}
}

// wantNoCommitsBetweenSkip checks that MR !3 was skipped because its branches
// have no commits between them.
func wantNoCommitsBetweenSkip(t *testing.T, result MergeRequestResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("migrateMergeRequest: %v", err)
	}
	if result.Status != StatusSkipped || !strings.Contains(result.SkipReason, "has no new commits") {
		t.Fatalf("result = %+v, want skipped because the branches have no commits between them", result)
	}
}

// migrateWithTempBranchesViaAPI migrates the merged MR !3 with
// -pull-requests-only, so its temporary branches are created by API. GitHub
// answers the creation of the pull request with createPR. It returns the
// result, the branches that were deleted and the error.
func migrateWithTempBranchesViaAPI(t *testing.T, createPR http.HandlerFunc) (MergeRequestResult, []string, error) {
	t.Helper()
	const startSHA = "1111111111111111111111111111111111111111"
	const endSHA = "2222222222222222222222222222222222222222"

	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/commits", []*gogitlab.Commit{
		{ID: startSHA, ShortID: startSHA[:8]},
		{ID: endSHA, ShortID: endSHA[:8]},
	}, &calls)
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/award_emoji", []*gogitlab.AwardEmoji{}, &calls)
	p := newGitLabTestProject(t, mux)
	p.m.cfg.PullRequestsOnly = true
	p.m.ghClient = &searchGitHub{}

	ghMux := http.NewServeMux()
	ghMux.HandleFunc("GET /repos/owner/repo/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"sha":%q,"parents":[{"sha":"0000000000000000000000000000000000000000"}]}`, r.PathValue("sha")); err != nil {
			t.Errorf("writing the commit: %v", err)
		}
	})
	var created, deleted []string
	ghMux.HandleFunc("POST /repos/owner/repo/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var ref gogithub.CreateRef
		if err := json.NewDecoder(r.Body).Decode(&ref); err != nil {
			t.Errorf("decoding the created reference: %v", err)
		}
		created = append(created, ref.Ref)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if _, err := fmt.Fprintf(w, `{"ref":%q}`, ref.Ref); err != nil {
			t.Errorf("writing the created reference: %v", err)
		}
	})
	ghMux.HandleFunc("DELETE /repos/owner/repo/git/refs/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, "refs/"+r.PathValue("ref"))
		w.WriteHeader(http.StatusNoContent)
	})
	ghMux.HandleFunc("POST /repos/owner/repo/pulls", createPR)
	useGitHubMux(t, p, ghMux)

	result, err := p.migrateMergeRequest(context.Background(), &gogitlab.BasicMergeRequest{
		IID: 3, Title: "some work", State: "merged", SourceBranch: "feature", TargetBranch: "main",
	})

	if want := []string{"refs/heads/migration-target-3/main", "refs/heads/migration-source-3/feature"}; !slices.Equal(created, want) {
		t.Fatalf("created branches = %v, want %v", created, want)
	}
	slices.Sort(deleted)
	return result, deleted, err
}

func TestMigrateMergeRequest_NoCommitsBetweenDeletesTemporaryBranchesViaAPI(t *testing.T) {
	// GitHub refuses the pull request because the temporary branches have no
	// commits between them, so both temporary branches must be deleted again.
	result, deleted, err := migrateWithTempBranchesViaAPI(t, noCommitsBetween(t, nil))
	wantNoCommitsBetweenSkip(t, result, err)

	if want := []string{"refs/heads/migration-source-3/feature", "refs/heads/migration-target-3/main"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted branches = %v, want %v", deleted, want)
	}
}

func TestMigrateMergeRequest_FailedPullRequestKeepsTemporaryBranchesViaAPI(t *testing.T) {
	// Any other error of the pull request creation fails the merge request,
	// and the temporary branches stay as before.
	result, deleted, err := migrateWithTempBranchesViaAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err == nil || !strings.Contains(err.Error(), "creating pull request") {
		t.Fatalf("migrateMergeRequest = %+v, %v, want the failed pull request creation", result, err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted branches = %v, want none", deleted)
	}
}

// remoteBranches returns the sorted branch names of the repository at dir. It
// reports errors with t.Errorf, so an HTTP handler of a test may call it.
func remoteBranches(t *testing.T, dir string) []string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Errorf("opening the remote repository: %v", err)
		return nil
	}
	refs, err := repo.Branches()
	if err != nil {
		t.Errorf("listing the remote branches: %v", err)
		return nil
	}
	var names []string
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		names = append(names, ref.Name().Short())
		return nil
	}); err != nil {
		t.Errorf("reading the remote branches: %v", err)
	}
	slices.Sort(names)
	return names
}

func TestMigrateMergeRequest_NoCommitsBetweenDeletesPushedTemporaryBranches(t *testing.T) {
	// MR !3 gets its temporary branches by a git push to the remote "github".
	// GitHub then refuses the pull request because the branches have no
	// commits between them, so both temporary branches must be deleted again.
	remoteDir := t.TempDir()
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("creating the remote repository: %v", err)
	}
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatalf("creating the local repository: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "github", URLs: []string{remoteDir}}); err != nil {
		t.Fatalf("adding the remote: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("opening the worktree: %v", err)
	}
	commit := func(msg string) plumbing.Hash {
		t.Helper()
		hash, err := worktree.Commit(msg, &git.CommitOptions{
			AllowEmptyCommits: true,
			Author:            &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
		})
		if err != nil {
			t.Fatalf("committing %q: %v", msg, err)
		}
		return hash
	}
	commit("base")
	mrCommit := commit("merge request work")

	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/commits", []*gogitlab.Commit{
		{ID: mrCommit.String(), ShortID: mrCommit.String()[:8]},
	}, &calls)
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/award_emoji", []*gogitlab.AwardEmoji{}, &calls)
	p := newGitLabTestProject(t, mux)
	p.repo = repo
	p.m.ghClient = &searchGitHub{}

	ghMux := http.NewServeMux()
	var pushedBeforeCreate []string
	ghMux.HandleFunc("POST /repos/owner/repo/pulls", noCommitsBetween(t, func() {
		pushedBeforeCreate = remoteBranches(t, remoteDir)
	}))
	useGitHubMux(t, p, ghMux)

	result, err := p.migrateMergeRequest(context.Background(), &gogitlab.BasicMergeRequest{
		IID: 3, Title: "some work", State: "merged", SourceBranch: "feature", TargetBranch: "main",
	})
	wantNoCommitsBetweenSkip(t, result, err)

	if want := []string{"migration-source-3/feature", "migration-target-3/main"}; !slices.Equal(pushedBeforeCreate, want) {
		t.Fatalf("remote branches when the pull request was created = %v, want %v", pushedBeforeCreate, want)
	}
	if got := remoteBranches(t, remoteDir); len(got) != 0 {
		t.Errorf("remote branches after the migration = %v, want none", got)
	}
}

// migrateExistingPullRequest migrates MR !3 from feature to master in the
// GitLab state mrState. Its pull request 5 exists on GitHub as pr. The GitLab
// trunk is master and the GitHub trunk is main, as with
// -rename-master-to-main. The test server answers every edit of pull request 5
// with the pull request after the edit, like GitHub does. It returns the edits
// in the order they were sent, each as the JSON object of the request, and the
// pull request after the migration.
func migrateExistingPullRequest(t *testing.T, mrState string, pr gogithub.PullRequest) ([]map[string]any, gogithub.PullRequest) {
	t.Helper()
	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/award_emoji", []*gogitlab.AwardEmoji{}, &calls)
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/notes", []*gogitlab.Note{}, &calls)
	p := newGitLabTestProject(t, mux)
	p.project.DefaultBranch = "master"
	p.defaultBranch = "main"

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("creating the local repository: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), plumbing.NewHash("1111111111111111111111111111111111111111"))); err != nil {
		t.Fatalf("creating the source branch: %v", err)
	}
	p.repo = repo

	pr.Number = Pointer(5)
	if pr.Body == nil {
		pr.Body = Pointer("> | **GitLab MR Number** | 3 |")
	}
	found := pr
	p.m.ghClient = &searchGitHub{
		issues: []*gogithub.Issue{{
			Number:           Pointer(5),
			PullRequestLinks: &gogithub.PullRequestLinks{URL: Pointer("https://api.github.com/repos/owner/repo/pulls/5")},
		}},
		prs: map[int]*gogithub.PullRequest{5: &found},
	}

	var edits []map[string]any
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("PATCH /repos/owner/repo/pulls/5", func(w http.ResponseWriter, r *http.Request) {
		var edit map[string]any
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
			t.Errorf("decoding the edit of the pull request: %v", err)
		}
		edits = append(edits, edit)
		if v, ok := edit["state"].(string); ok {
			pr.State = Pointer(v)
		}
		if v, ok := edit["title"].(string); ok {
			pr.Title = Pointer(v)
		}
		if v, ok := edit["body"].(string); ok {
			pr.Body = Pointer(v)
		}
		if v, ok := edit["base"].(string); ok {
			pr.Base = &gogithub.PullRequestBranch{Ref: Pointer(v)}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(pr); err != nil {
			t.Errorf("writing the edited pull request: %v", err)
		}
	})
	ghMux.HandleFunc("GET /repos/owner/repo/issues/5/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("writing the pull request comments: %v", err)
		}
	})
	useGitHubMux(t, p, ghMux)

	result, err := p.migrateMergeRequest(context.Background(), &gogitlab.BasicMergeRequest{
		IID: 3, Title: "some work", State: mrState, SourceBranch: "feature", TargetBranch: "master",
	})
	if err != nil {
		t.Fatalf("migrateMergeRequest: %v", err)
	}
	if result.Status != StatusSuccess {
		t.Fatalf("status = %q, want %q", result.Status, StatusSuccess)
	}
	return edits, pr
}

// openPullRequest returns pull request 5 as open with base branch base, with
// the title and draft flag of MR !3.
func openPullRequest(base string) gogithub.PullRequest {
	return gogithub.PullRequest{
		State: Pointer("open"),
		Title: Pointer("some work"),
		Draft: Pointer(false),
		Base:  &gogithub.PullRequestBranch{Ref: Pointer(base)},
	}
}

func TestMigrateMergeRequest_OpenPullRequestGetsRenamedTrunkAsBase(t *testing.T) {
	// The pull request of the open MR !3 still targets the old trunk master,
	// so its base branch must change to the new trunk main.
	edits, pr := migrateExistingPullRequest(t, "opened", openPullRequest("master"))
	if len(edits) != 1 || edits[0]["base"] != "main" {
		t.Fatalf("edits = %v, want one edit with base main", edits)
	}
	if got := pr.GetBase().GetRef(); got != "main" {
		t.Errorf("base branch = %q, want main", got)
	}
}

func TestMigrateMergeRequest_OnlyBaseOfOpenPullRequestIsOutOfDate(t *testing.T) {
	// A first migration writes the current body of MR !3.
	_, migrated := migrateExistingPullRequest(t, "opened", openPullRequest("main"))

	// With that body and the new trunk as base, nothing is out of date.
	upToDate := openPullRequest("main")
	upToDate.Body = migrated.Body
	if edits, _ := migrateExistingPullRequest(t, "opened", upToDate); len(edits) != 0 {
		t.Fatalf("edits of an up to date pull request = %v, want none", edits)
	}

	// Only the base branch is out of date, which alone needs an edit.
	oldBase := openPullRequest("master")
	oldBase.Body = migrated.Body
	edits, _ := migrateExistingPullRequest(t, "opened", oldBase)
	if len(edits) != 1 || edits[0]["base"] != "main" {
		t.Fatalf("edits = %v, want one edit with base main", edits)
	}
}

func TestMigrateMergeRequest_ClosedPullRequestKeepsItsBase(t *testing.T) {
	// The pull request of the merged MR !3 is closed and targets the
	// temporary branch from its migration. GitHub refuses a new base for a
	// closed pull request, so an up to date pull request gets no edit.
	closed := gogithub.PullRequest{
		State: Pointer("closed"),
		Title: Pointer("some work"),
		Draft: Pointer(false),
		Base:  &gogithub.PullRequestBranch{Ref: Pointer("migration-target-3/master")},
	}
	_, migrated := migrateExistingPullRequest(t, "merged", closed)

	closed.Body = migrated.Body
	edits, pr := migrateExistingPullRequest(t, "merged", closed)
	if len(edits) != 0 {
		t.Fatalf("edits = %v, want none", edits)
	}
	if got := pr.GetBase().GetRef(); got != "migration-target-3/master" {
		t.Errorf("base branch = %q, want migration-target-3/master", got)
	}
}

func TestMigrateMergeRequest_BaseChangesAfterTheStateChange(t *testing.T) {
	t.Run("reopened merge request", func(t *testing.T) {
		// MR !3 is open again, so its closed pull request is reopened first
		// and then gets the new trunk as base.
		closed := openPullRequest("master")
		closed.State = Pointer("closed")
		edits, pr := migrateExistingPullRequest(t, "opened", closed)
		if len(edits) != 2 || edits[0]["state"] != "open" || edits[0]["base"] != nil || edits[1]["base"] != "main" {
			t.Fatalf("edits = %v, want the reopening without a base, then an edit with base main", edits)
		}
		if got := pr.GetBase().GetRef(); got != "main" {
			t.Errorf("base branch = %q, want main", got)
		}
	})

	t.Run("merged merge request", func(t *testing.T) {
		// MR !3 was merged, so its open pull request is closed first and
		// then keeps its base.
		edits, pr := migrateExistingPullRequest(t, "merged", openPullRequest("master"))
		if len(edits) == 0 || edits[0]["state"] != "closed" {
			t.Fatalf("edits = %v, want the closing first", edits)
		}
		for i, edit := range edits {
			if edit["base"] != nil {
				t.Errorf("edit %d = %v, want no base for the closed pull request", i, edit)
			}
		}
		if got := pr.GetBase().GetRef(); got != "master" {
			t.Errorf("base branch = %q, want master", got)
		}
	})
}

// serveOpenPullRequests adds to ghMux the open pull requests 5 and 6 on base,
// listed on two pages. Each edit of a pull request calls onEdit with the JSON
// object of the request and the number of the pull request under "number".
// GitHub answers the edit with editStatus.
func serveOpenPullRequests(t *testing.T, ghMux *http.ServeMux, base string, editStatus int, onEdit func(edit map[string]any)) {
	t.Helper()
	ghMux.HandleFunc("GET /repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("state") != "open" || query.Get("base") != base {
			t.Errorf("listed pull requests with %v, want the open ones on %s", query, base)
		}
		prs := []*gogithub.PullRequest{{Number: Pointer(5)}}
		if query.Get("page") == "2" {
			prs = []*gogithub.PullRequest{{Number: Pointer(6)}}
		} else {
			query.Set("page", "2")
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, query.Encode()))
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(prs); err != nil {
			t.Errorf("writing the pull requests: %v", err)
		}
	})
	ghMux.HandleFunc("PATCH /repos/owner/repo/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var edit map[string]any
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
			t.Errorf("decoding the edit of the pull request: %v", err)
		}
		edit["number"] = r.PathValue("number")
		onEdit(edit)
		w.Header().Set("Content-Type", "application/json")
		if editStatus != http.StatusOK {
			w.WriteHeader(editStatus)
			if _, err := fmt.Fprint(w, `{"message":"Validation Failed"}`); err != nil {
				t.Errorf("writing the edit error: %v", err)
			}
			return
		}
		if _, err := fmt.Fprintf(w, `{"number":%s}`, r.PathValue("number")); err != nil {
			t.Errorf("writing the edited pull request: %v", err)
		}
	})
}

// retargetBeforeTrim calls retargetPullRequestsBeforeTrim for the GitLab trunk
// master and the GitHub trunk githubTrunk, with githubDefault as the default
// branch of the GitHub repository before the run and branchesToDelete as the
// branches the trim deletes. The test server lists the open pull requests 5
// and 6 on githubDefault, on two pages. It returns the edits in the order they
// were sent, each with the number of the pull request and the JSON object of
// the request.
func retargetBeforeTrim(t *testing.T, githubTrunk, githubDefault string, branchesToDelete []string) ([]map[string]any, error) {
	t.Helper()
	p := newGitLabTestProject(t, http.NewServeMux())
	p.project.DefaultBranch = "master"
	p.defaultBranch = githubTrunk

	var edits []map[string]any
	ghMux := http.NewServeMux()
	serveOpenPullRequests(t, ghMux, githubDefault, http.StatusOK, func(edit map[string]any) {
		edits = append(edits, edit)
	})
	useGitHubMux(t, p, ghMux)

	err := p.retargetPullRequestsBeforeTrim(context.Background(), branchesToDelete, githubDefault)
	return edits, err
}

func TestRetargetPullRequestsBeforeTrim_OldTrunkIsTrimmed(t *testing.T) {
	// The trim deletes master, which closes the open pull requests on it. So
	// they get the new trunk main as base first, and the edit changes nothing
	// else.
	edits, err := retargetBeforeTrim(t, "main", "master", []string{"stale", "master"})
	if err != nil {
		t.Fatalf("retargetPullRequestsBeforeTrim: %v", err)
	}
	want := []map[string]any{{"number": "5", "base": "main"}, {"number": "6", "base": "main"}}
	if fmt.Sprint(edits) != fmt.Sprint(want) {
		t.Fatalf("edits = %v, want %v", edits, want)
	}
}

func TestRetargetPullRequestsBeforeTrim_OldDefaultBranchIsTrimmed(t *testing.T) {
	// An earlier run with -rename-master-to-main made main the default branch
	// on GitHub. This run has no rename, so master becomes the default branch
	// again and the trim deletes main, which closes the open pull requests on
	// it. So they get master as base first.
	edits, err := retargetBeforeTrim(t, "master", "main", []string{"stale", "main"})
	if err != nil {
		t.Fatalf("retargetPullRequestsBeforeTrim: %v", err)
	}
	want := []map[string]any{{"number": "5", "base": "master"}, {"number": "6", "base": "master"}}
	if fmt.Sprint(edits) != fmt.Sprint(want) {
		t.Fatalf("edits = %v, want %v", edits, want)
	}
}

func TestRetargetPullRequestsBeforeTrim_NothingToDo(t *testing.T) {
	t.Run("old trunk is not trimmed", func(t *testing.T) {
		edits, err := retargetBeforeTrim(t, "main", "master", []string{"stale"})
		if err != nil || len(edits) != 0 {
			t.Fatalf("edits = %v, err = %v, want no edits and no error", edits, err)
		}
	})
	t.Run("trunk is not renamed", func(t *testing.T) {
		edits, err := retargetBeforeTrim(t, "master", "master", []string{"stale", "master"})
		if err != nil || len(edits) != 0 {
			t.Fatalf("edits = %v, err = %v, want no edits and no error", edits, err)
		}
	})
}

// updateBranchesAfterRename calls updateGithubBranches as a migration with
// -rename-master-to-main and -trim-branches-on-github does after the push of
// the branches. The GitHub repository is a local repository with the branches
// main, master and stale, as after an earlier run without the rename that made
// master the default branch. Only main was migrated now. GitHub answers the
// edits of the open pull requests 5 and 6 on master with editStatus. A new
// default branch becomes the HEAD of the local repository. It returns the
// GitHub API calls in the order they were sent, each with the remote
// branches at that time, the remote branches afterwards and the error.
func updateBranchesAfterRename(t *testing.T, editStatus int) ([]string, []string, error) {
	t.Helper()
	remoteDir := t.TempDir()
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("creating the remote repository: %v", err)
	}
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatalf("creating the local repository: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "github", URLs: []string{remoteDir}}); err != nil {
		t.Fatalf("adding the remote: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("opening the worktree: %v", err)
	}
	if _, err := worktree.Commit("base", &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("committing: %v", err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "github", RefSpecs: []gitconfig.RefSpec{
		"refs/heads/master:refs/heads/main",
		"refs/heads/master:refs/heads/master",
		"refs/heads/master:refs/heads/stale",
	}}); err != nil {
		t.Fatalf("pushing the branches of the earlier run: %v", err)
	}

	p := newGitLabTestProject(t, http.NewServeMux())
	p.repo = repo
	p.project.DefaultBranch = "master"
	p.defaultBranch = "main"
	p.m.cfg.TrimGithubBranches = true
	p.result.BranchesMigrated = []string{"main"}
	p.m.ghClient = &searchGitHub{branches: []*gogithub.Branch{
		{Name: Pointer("main")}, {Name: Pointer("master")}, {Name: Pointer("stale")},
	}}

	var calls []string
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("PATCH /repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		var edit map[string]any
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
			t.Errorf("decoding the edit of the repository: %v", err)
		}
		calls = append(calls, fmt.Sprintf("default branch %v, remote branches %v", edit["default_branch"], remoteBranches(t, remoteDir)))
		// Like GitHub, git refuses to delete the current branch of the
		// remote, so the default branch becomes its HEAD.
		if name, ok := edit["default_branch"].(string); ok {
			remote, err := git.PlainOpen(remoteDir)
			if err != nil {
				t.Errorf("opening the remote repository: %v", err)
			} else if err := remote.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(name))); err != nil {
				t.Errorf("setting the default branch of the remote repository: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{}`); err != nil {
			t.Errorf("writing the edited repository: %v", err)
		}
	})
	serveOpenPullRequests(t, ghMux, "master", editStatus, func(edit map[string]any) {
		calls = append(calls, fmt.Sprintf("base of pull request %v %v, remote branches %v", edit["number"], edit["base"], remoteBranches(t, remoteDir)))
	})
	useGitHubMux(t, p, ghMux)

	err = p.updateGithubBranches(context.Background(), "https://github.com/owner/repo", "master")
	return calls, remoteBranches(t, remoteDir), err
}

func TestUpdateGithubBranches_OldTrunkIsTrimmedLast(t *testing.T) {
	// GitHub refuses to delete its default branch master, and closes the open
	// pull requests on master when it is deleted. So main becomes the default
	// branch and the base of the pull requests first, and only then the trim
	// deletes master and stale.
	calls, branches, err := updateBranchesAfterRename(t, http.StatusOK)
	if err != nil {
		t.Fatalf("updateGithubBranches: %v", err)
	}
	want := []string{
		"default branch main, remote branches [main master stale]",
		"base of pull request 5 main, remote branches [main master stale]",
		"base of pull request 6 main, remote branches [main master stale]",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("GitHub API calls =\n%v\nwant\n%v", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{"main"}; !slices.Equal(branches, want) {
		t.Errorf("remote branches after the trim = %v, want %v", branches, want)
	}
}

func TestUpdateGithubBranches_RefusedBaseChangeSkipsTheTrim(t *testing.T) {
	// GitHub refuses the new base of pull request 5. The trim would close it,
	// so no branch is deleted and the error names the pull request.
	calls, branches, err := updateBranchesAfterRename(t, http.StatusUnprocessableEntity)
	if err == nil || !strings.Contains(err.Error(), "changing base branch of pull request 5") {
		t.Fatalf("updateGithubBranches = %v, want the refused base change of pull request 5", err)
	}
	if len(calls) != 2 {
		t.Errorf("GitHub API calls = %v, want the default branch and pull request 5", calls)
	}
	if want := []string{"main", "master", "stale"}; !slices.Equal(branches, want) {
		t.Errorf("remote branches = %v, want %v", branches, want)
	}
}
