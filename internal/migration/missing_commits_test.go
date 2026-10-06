package migration

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os/exec"
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
	"github.com/hashicorp/go-hclog"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// gitlabWithHiddenMergeRequest creates a bare repository that plays the GitLab
// project. Its branch main has one commit. The two commits of a merge request
// follow it, but no branch points to them: only refs/keep-around/<end commit>
// does, and upload-pack hides that ref, like a GitLab project that keeps the
// commits of a merge request with a deleted source branch only under such a
// hidden ref. wantOption is an option of upload-pack that is set to true, for
// example allowAnySHA1InWant, so that it serves commits that it does not
// advertise; "" sets none. It returns the folder and the start and end commit.
func gitlabWithHiddenMergeRequest(t *testing.T, wantOption string) (string, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	gitlab, err := git.PlainInit(dir, true)
	if err != nil {
		t.Fatalf("creating the GitLab repository: %v", err)
	}
	cfg, err := gitlab.Config()
	if err != nil {
		t.Fatalf("reading the GitLab repository config: %v", err)
	}
	cfg.Raw.Section("uploadpack").SetOption("hideRefs", "refs/keep-around")
	if wantOption != "" {
		cfg.Raw.Section("uploadpack").SetOption(wantOption, "true")
	}
	if err := gitlab.SetConfig(cfg); err != nil {
		t.Fatalf("writing the GitLab repository config: %v", err)
	}

	work, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("creating the work repository: %v", err)
	}
	commit := func(msg string, parents ...plumbing.Hash) plumbing.Hash {
		t.Helper()
		tree := work.Storer.NewEncodedObject()
		tree.SetType(plumbing.TreeObject)
		treeHash, err := work.Storer.SetEncodedObject(tree)
		if err != nil {
			t.Fatalf("storing the empty tree: %v", err)
		}
		sig := object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
		c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: treeHash, ParentHashes: parents}
		obj := work.Storer.NewEncodedObject()
		if err := c.Encode(obj); err != nil {
			t.Fatalf("encoding commit %q: %v", msg, err)
		}
		hash, err := work.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("storing commit %q: %v", msg, err)
		}
		return hash
	}
	base := commit("base")
	start := commit("merge request start", base)
	end := commit("merge request end", start)

	if _, err := work.CreateRemote(&gitconfig.RemoteConfig{Name: "gitlab", URLs: []string{dir}}); err != nil {
		t.Fatalf("adding the GitLab remote: %v", err)
	}
	if err := work.Push(&git.PushOptions{RemoteName: "gitlab", RefSpecs: []gitconfig.RefSpec{
		gitconfig.RefSpec(base.String() + ":refs/heads/main"),
		gitconfig.RefSpec(end.String() + ":refs/keep-around/" + end.String()),
	}}); err != nil {
		t.Fatalf("pushing to the GitLab repository: %v", err)
	}
	if err := gitlab.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("pointing HEAD of the GitLab repository to main: %v", err)
	}
	return dir, start, end
}

// remoteRefs returns the sorted names of all references of the bare
// repository at dir, without HEAD. It reports errors with t.Errorf, so an HTTP
// handler of a test may call it.
func remoteRefs(t *testing.T, dir string) []string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Errorf("opening the remote repository: %v", err)
		return nil
	}
	refs, err := repo.References()
	if err != nil {
		t.Errorf("listing the remote references: %v", err)
		return nil
	}
	var names []string
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name() != plumbing.HEAD {
			names = append(names, ref.Name().String())
		}
		return nil
	}); err != nil {
		t.Errorf("reading the remote references: %v", err)
	}
	slices.Sort(names)
	return names
}

type hiddenCommitsRun struct {
	result MergeRequestResult
	// pushed holds the references of the GitHub repository when the pull
	// request was created, nil when it was not created.
	pushed []string
	logs   string
	err    error
}

// hiddenCommitsSetup configures migrateWithHiddenMergeRequestCommits.
type hiddenCommitsSetup struct {
	// wantOption is passed to gitlabWithHiddenMergeRequest.
	wantOption string
	// unknownCommits makes the commit list of MR !3 name commits that the
	// GitLab repository does not have, like commits that GitLab removed by
	// garbage collection.
	unknownCommits bool
	// skipInvalid sets -skip-invalid-merge-requests.
	skipInvalid bool
	// fetchHandler, when it is not nil, returns the HTTP handler that answers
	// the fetch of the missing commits: after the clone, the remote "gitlab"
	// points to it, with the GitLab token in the URL like the clone URL of
	// mirrorRepository. backend serves the GitLab repository with
	// git http-backend, and cancel stops the migration.
	fetchHandler func(cancel context.CancelFunc, backend http.Handler) http.HandlerFunc
}

// fetchTestToken is the GitLab token of migrateWithHiddenMergeRequestCommits.
const fetchTestToken = "glpat-secret-fetch-token"

// migrateWithHiddenMergeRequestCommits clones the repository of
// gitlabWithHiddenMergeRequest like mirrorRepository does, so the clone lacks
// the commits of the merged MR !3, and migrates MR !3 in normal mode.
func migrateWithHiddenMergeRequestCommits(t *testing.T, setup hiddenCommitsSetup) hiddenCommitsRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gitlabDir, start, end := gitlabWithHiddenMergeRequest(t, setup.wantOption)
	repo, err := git.CloneContext(context.Background(), memory.NewStorage(), nil, &git.CloneOptions{
		URL:        gitlabDir,
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		t.Fatalf("cloning the GitLab repository: %v", err)
	}
	if _, err := object.GetCommit(repo.Storer, end); err == nil {
		t.Fatalf("the clone has the end commit %s, want it missing", end)
	}
	if setup.fetchHandler != nil {
		gitPath, err := exec.LookPath("git")
		if err != nil {
			t.Skipf("git http-backend serves the fetch, but git is not installed: %v", err)
		}
		backend := &cgi.Handler{
			Path: gitPath,
			Args: []string{"http-backend"},
			Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(gitlabDir), "GIT_HTTP_EXPORT_ALL=1"},
		}
		srv := httptest.NewServer(setup.fetchHandler(cancel, backend))
		t.Cleanup(srv.Close)
		fetchURL, err := url.Parse(srv.URL + "/" + filepath.Base(gitlabDir))
		if err != nil {
			t.Fatalf("parsing the fetch URL: %v", err)
		}
		fetchURL.User = url.UserPassword("oauth2", fetchTestToken)
		if err := repo.DeleteRemote("gitlab"); err != nil {
			t.Fatalf("deleting the GitLab remote: %v", err)
		}
		if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "gitlab", URLs: []string{fetchURL.String()}}); err != nil {
			t.Fatalf("pointing the GitLab remote to the fetch server: %v", err)
		}
	}
	githubDir := t.TempDir()
	if _, err := git.PlainInit(githubDir, true); err != nil {
		t.Fatalf("creating the GitHub repository: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "github", URLs: []string{githubDir}, Mirror: true}); err != nil {
		t.Fatalf("adding the GitHub remote: %v", err)
	}

	if setup.unknownCommits {
		start = plumbing.NewHash(strings.Repeat("1", 40))
		end = plumbing.NewHash(strings.Repeat("2", 40))
	}
	mux := http.NewServeMux()
	var calls atomic.Int32
	// GitLab lists the newest commit first.
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/commits", []*gogitlab.Commit{
		{ID: end.String(), ShortID: end.String()[:8]},
		{ID: start.String(), ShortID: start.String()[:8]},
	}, &calls)
	servePages(t, mux, "/api/v4/projects/1/merge_requests/3/award_emoji", []*gogitlab.AwardEmoji{}, &calls)
	p := newGitLabTestProject(t, mux)
	p.m.cfg.SkipInvalidMergeRequests = setup.skipInvalid
	p.m.cfg.GitlabToken = fetchTestToken
	var logs strings.Builder
	p.log = hclog.New(&hclog.LoggerOptions{Output: &logs, Level: hclog.Trace})
	p.repo = repo
	p.m.ghClient = &searchGitHub{}

	var run hiddenCommitsRun
	ghMux := http.NewServeMux()
	serveCreatedPullRequest(t, mux, ghMux, &calls)
	ghMux.HandleFunc("POST /repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		run.pushed = remoteRefs(t, githubDir)
		createdPullRequest(t)(w, r)
	})
	useGitHubMux(t, p, ghMux)

	run.result, run.err = p.migrateMergeRequest(ctx, &gogitlab.BasicMergeRequest{
		IID: 3, Title: "some work", State: "merged", SourceBranch: "feature", TargetBranch: "main",
	})
	run.logs = logs.String()
	return run
}

func TestMigrateMergeRequest_FetchesCommitsMissingFromTheClone(t *testing.T) {
	// The source branch of MR !3 was deleted on GitLab, so the mirror clone
	// lacks its commits (issue #137). The end commit is fetched by SHA and
	// brings the start commit as its ancestor, the pull request is created, and
	// only the two temporary branches reach GitHub, not the references of the
	// fetched commits. allowTipSHA1InWant serves only the tip of the hidden
	// ref, which is the end commit, so one fetch must get both commits.
	for _, option := range []string{"allowAnySHA1InWant", "allowTipSHA1InWant"} {
		t.Run(option, func(t *testing.T) {
			run := migrateWithHiddenMergeRequestCommits(t, hiddenCommitsSetup{wantOption: option})
			if run.err != nil {
				t.Fatalf("migrateMergeRequest: %v", run.err)
			}
			if run.result.Status != StatusSuccess || run.result.GitHubPRNumber == nil || *run.result.GitHubPRNumber != 7 {
				t.Fatalf("result = %+v, want pull request 7 migrated", run.result)
			}
			if want := []string{"refs/heads/migration-source-3/feature", "refs/heads/migration-target-3/main"}; !slices.Equal(run.pushed, want) {
				t.Errorf("GitHub references when the pull request was created = %v, want %v", run.pushed, want)
			}
			if n := strings.Count(run.logs, "fetching a commit of the merge request"); n != 1 {
				t.Errorf("the log has %d fetches, want 1:\n%s", n, run.logs)
			}
			wantNoErrorOrWarning(t, run.logs)
		})
	}
}

func TestMigrateMergeRequest_UnfetchableMissingCommitNamesTheCause(t *testing.T) {
	// The server does not serve commits that it does not advertise, so the
	// commits of MR !3 cannot be fetched. The error names the cause, the
	// failed fetch and -skip-invalid-merge-requests.
	run := migrateWithHiddenMergeRequestCommits(t, hiddenCommitsSetup{})
	if run.err == nil {
		t.Fatalf("migrateMergeRequest = %+v, want an error", run.result)
	}
	for _, want := range []string{"loading start commit", "object not found", "source branch was deleted", "fetching it from GitLab by its SHA failed", "-skip-invalid-merge-requests"} {
		if !strings.Contains(run.err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", run.err, want)
		}
	}
	if run.pushed != nil {
		t.Errorf("the pull request was created with the GitHub references %v, want no pull request", run.pushed)
	}
}

func TestMigrateMergeRequest_UnfetchableMissingCommitIsSkipped(t *testing.T) {
	// With -skip-invalid-merge-requests the merge request whose commits cannot
	// be fetched is skipped, and the skip reason names the failed fetch. Over
	// HTTP, the refusal of the server must not count as an error that a later
	// run can get past. The refusal is either go-git's own (the server offers
	// no fetch by SHA) or the server's "not our ref" (the server allows any SHA,
	// but does not have the commit, for example because GitLab removed it by
	// garbage collection).
	httpBackend := func(_ context.CancelFunc, backend http.Handler) http.HandlerFunc { return backend.ServeHTTP }
	for name, tc := range map[string]struct {
		setup      hiddenCommitsSetup
		wantReason string
	}{
		"file/no SHA wants": {setup: hiddenCommitsSetup{}},
		"http/no SHA wants": {setup: hiddenCommitsSetup{fetchHandler: httpBackend}},
		"file/unknown commit": {
			setup:      hiddenCommitsSetup{wantOption: "allowAnySHA1InWant", unknownCommits: true},
			wantReason: "not our ref",
		},
		"http/unknown commit": {
			setup:      hiddenCommitsSetup{wantOption: "allowAnySHA1InWant", unknownCommits: true, fetchHandler: httpBackend},
			wantReason: "not our ref",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.setup.skipInvalid = true
			run := migrateWithHiddenMergeRequestCommits(t, tc.setup)
			if run.err != nil {
				t.Fatalf("migrateMergeRequest: %v", run.err)
			}
			if run.result.Status != StatusSkipped || !strings.HasPrefix(run.result.SkipReason, "start commit is not in the clone and fetching it from GitLab failed: ") {
				t.Fatalf("result = %+v, want skipped because the start commit could not be fetched", run.result)
			}
			if !strings.Contains(run.result.SkipReason, tc.wantReason) {
				t.Errorf("skip reason = %q, want it to contain %q", run.result.SkipReason, tc.wantReason)
			}
		})
	}
}

func TestMigrateMergeRequest_StopDuringFetchOfMissingCommitIsNotSkipped(t *testing.T) {
	// The run is stopped while the pack with the commits of MR !3 is
	// downloaded. go-git returns an error for that download that does not
	// wrap the stop. With -skip-invalid-merge-requests the merge request must
	// not be skipped: with -state-dir a skip is never migrated again,
	// although the next run could fetch the commits.
	run := migrateWithHiddenMergeRequestCommits(t, hiddenCommitsSetup{
		wantOption:  "allowAnySHA1InWant",
		skipInvalid: true,
		fetchHandler: func(cancel context.CancelFunc, backend http.Handler) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					cancel()
					w.WriteHeader(http.StatusTeapot)
					return
				}
				backend.ServeHTTP(w, r)
			}
		},
	})
	if run.err == nil || run.result.Status == StatusSkipped {
		t.Fatalf("migrateMergeRequest = %+v, %v, want an error and no skip", run.result, run.err)
	}
}

func TestMigrateMergeRequest_CutPackDuringFetchOfMissingCommitIsNotSkipped(t *testing.T) {
	// A proxy in front of GitLab closes the connection while the pack with the
	// commits of MR !3 is downloaded: the response declares its full length,
	// but only half of it arrives. The body then ends with an unexpected EOF,
	// which is no net.Error and no HTTP status. That can work in the next run,
	// so with -skip-invalid-merge-requests the merge request must fail and not
	// be skipped: with -state-dir a skip is never migrated again.
	run := migrateWithHiddenMergeRequestCommits(t, hiddenCommitsSetup{
		wantOption:  "allowAnySHA1InWant",
		skipInvalid: true,
		fetchHandler: func(cancel context.CancelFunc, backend http.Handler) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					backend.ServeHTTP(w, r)
					return
				}
				rec := httptest.NewRecorder()
				backend.ServeHTTP(rec, r)
				body := rec.Body.Bytes()
				for name, values := range rec.Header() {
					w.Header()[name] = values
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(rec.Code)
				_, _ = w.Write(body[:len(body)/2])
			}
		},
	})
	if run.err == nil || run.result.Status == StatusSkipped {
		t.Fatalf("migrateMergeRequest = %+v, %v, want an error and no skip", run.result, run.err)
	}
	if !strings.Contains(run.err.Error(), "a later run can try again") {
		t.Errorf("error = %q, want it to say that a later run can try again", run.err)
	}
}

func TestMigrateMergeRequest_ServerErrorDuringFetchOfMissingCommit(t *testing.T) {
	// GitLab answers a request of the fetch of the commits of MR !3 with 502.
	// That can work in the next run, so with -skip-invalid-merge-requests the
	// merge request fails and is not skipped: with -state-dir a skip is never
	// migrated again. go-git puts the URL of the request into the error, and
	// the URL holds the GitLab token. The error goes to the log, the report
	// and the state file, so it must not show the token.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			run := migrateWithHiddenMergeRequestCommits(t, hiddenCommitsSetup{
				wantOption:  "allowAnySHA1InWant",
				skipInvalid: true,
				fetchHandler: func(cancel context.CancelFunc, backend http.Handler) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						if r.Method == method {
							w.WriteHeader(http.StatusBadGateway)
							return
						}
						backend.ServeHTTP(w, r)
					}
				},
			})
			if run.err == nil || run.result.Status == StatusSkipped {
				t.Fatalf("migrateMergeRequest = %+v, %v, want an error and no skip", run.result, run.err)
			}
			if !strings.Contains(run.err.Error(), "502") {
				t.Errorf("error = %q, want it to name the status 502", run.err)
			}
			for name, text := range map[string]string{"error": run.err.Error(), "log": run.logs, "skip reason": run.result.SkipReason, "result error": run.result.Error} {
				if strings.Contains(text, fetchTestToken) {
					t.Errorf("the %s shows the GitLab token: %q", name, text)
				}
			}
		})
	}
}
