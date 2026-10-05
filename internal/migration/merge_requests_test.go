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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	t.Run("listing projects", func(t *testing.T) {
		mux := http.NewServeMux()
		p := newGitLabTestProject(t, mux)
		started := make(chan struct{}, 1)
		serveUntilCanceled(t, mux, "/api/v4/projects", started)

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
		var calls atomic.Int32
		servePages(t, mux, "/api/v4/projects", []*gogitlab.Project{{ID: 1, PathWithNamespace: "group/project"}}, &calls)
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
