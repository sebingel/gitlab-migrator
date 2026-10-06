package migration

import (
	"context"
	"net/http"
	"slices"
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

// migrateWithHiddenMergeRequestCommits clones the repository of
// gitlabWithHiddenMergeRequest like mirrorRepository does, so the clone lacks
// the commits of the merged MR !3, and migrates MR !3 in normal mode.
func migrateWithHiddenMergeRequestCommits(t *testing.T, wantOption string, skipInvalid bool) hiddenCommitsRun {
	t.Helper()
	gitlabDir, start, end := gitlabWithHiddenMergeRequest(t, wantOption)
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
	githubDir := t.TempDir()
	if _, err := git.PlainInit(githubDir, true); err != nil {
		t.Fatalf("creating the GitHub repository: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "github", URLs: []string{githubDir}, Mirror: true}); err != nil {
		t.Fatalf("adding the GitHub remote: %v", err)
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
	p.m.cfg.SkipInvalidMergeRequests = skipInvalid
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

	run.result, run.err = p.migrateMergeRequest(context.Background(), &gogitlab.BasicMergeRequest{
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
			run := migrateWithHiddenMergeRequestCommits(t, option, false)
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
	run := migrateWithHiddenMergeRequestCommits(t, "", false)
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
	// be fetched is skipped, and the skip reason names the failed fetch.
	run := migrateWithHiddenMergeRequestCommits(t, "", true)
	if run.err != nil {
		t.Fatalf("migrateMergeRequest: %v", run.err)
	}
	if run.result.Status != StatusSkipped || !strings.HasPrefix(run.result.SkipReason, "start commit is not in the clone and fetching it from GitLab failed: ") {
		t.Fatalf("result = %+v, want skipped because the start commit could not be fetched", run.result)
	}
}
