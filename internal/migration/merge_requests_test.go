package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v84/github"
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

			got, err := p.listMergeRequestCommits(7)
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

func TestListMergeRequestAwardEmoji_ReadsAllPages(t *testing.T) {
	// A thumbs up after the first 100 award emoji is on the second page.
	const total = 130
	awards := make([]*gogitlab.AwardEmoji, total)
	for i := range awards {
		awards[i] = &gogitlab.AwardEmoji{ID: i + 1, Name: "rocket"}
	}
	awards[119].Name = "thumbsup"
	awards[119].User.Username = "late-approver"

	mux := http.NewServeMux()
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests/7/award_emoji", awards, &calls)
	p := newGitLabTestProject(t, mux)

	got, err := p.listMergeRequestAwardEmoji(7)
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

func TestMigrateMergeRequests_OpenMergeRequestSkipIsNotFinal(t *testing.T) {
	// migrate runs migrateMergeRequests like a new process of the tool: it
	// loads the state file and migrates MR !3 in the given GitLab state. It
	// returns the results and the number of GitHub searches, which is 1 when
	// the merge request was really processed.
	migrate := func(t *testing.T, statePath, mrState string, skipOpen bool) ([]MergeRequestResult, int) {
		t.Helper()
		mux := http.NewServeMux()
		var calls atomic.Int32
		servePages(t, mux, "/api/v4/projects/1/merge_requests", []*gogitlab.MergeRequest{
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

		return p.migrateMergeRequests(context.Background()), gh.searches
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
