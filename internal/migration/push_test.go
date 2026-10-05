package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	"github.com/sebingel/gitlab-migrator/internal/clients"
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestPushErrHint(t *testing.T) {
	const (
		noForceHint  = " (hint: remove -no-force if push is rejected due to conflicts)"
		workflowHint = " (hint: add 'workflow' scope to your GitHub token to push workflow files)"
	)

	// nonFastForward is the error go-git returns when its own check before a
	// push without force finds that the remote branch is not an ancestor.
	nonFastForward := errors.New("non-fast-forward update: refs/heads/main")

	// remoteRejected returns the error go-git returns when the remote rejects
	// the update of ref with reason.
	remoteRejected := func(ref, reason string) error {
		return (&packp.CommandStatus{ReferenceName: plumbing.ReferenceName(ref), Status: reason}).Error()
	}

	// The text matches the existing workflow scope check in pushErrHint.
	workflowScope := remoteRejected("refs/heads/main", "refusing to allow a Personal Access Token to create or update workflow .github/workflows/ci.yml without 'workflow' scope")

	tests := []struct {
		name    string
		noForce bool
		err     error
		want    string
	}{
		{"nil error", true, nil, ""},
		{"non-fast-forward found by go-git", true, nonFastForward, noForceHint},
		{"non-fast-forward rejected by the remote", true, remoteRejected("refs/heads/main", "non-fast-forward"), noForceHint},
		{"rejected by the remote for another reason", true, remoteRejected("refs/heads/main", "protected branch hook declined"), ""},
		{"branch name contains non-fast-forward", true, remoteRejected("refs/heads/fix/non-fast-forward", "protected branch hook declined"), ""},
		{"missing workflow scope", true, workflowScope, workflowHint},
		// go-git returns this for an annotated tag that differs on the remote,
		// but also for other missing objects, so it gets no hint.
		{"object not found", true, plumbing.ErrObjectNotFound, ""},
		{"authentication required", true, transport.ErrAuthenticationRequired, ""},
		{"authorization failed", true, transport.ErrAuthorizationFailed, ""},
		{"network error", true, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, ""},
		{"non-fast-forward with force push", false, nonFastForward, ""},
		{"missing workflow scope with force push", false, workflowScope, workflowHint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &project{m: &Migrator{cfg: &config.Config{NoForce: tt.noForce}}}
			if got := p.pushErrHint(tt.err); got != tt.want {
				t.Errorf("pushErrHint(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestPushToGitHub_SetsDefaultBranchBeforeTrim(t *testing.T) {
	// An earlier run pushed the GitLab trunk "master" to GitHub, where it is
	// the default branch. This run renames the trunk to "main" and trims the
	// GitHub branches. GitHub cannot delete its default branch, so the new
	// trunk must be pushed and made the default before the trim deletes
	// "master". The local remote plays GitHub: its HEAD is the default branch.
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
	trunk, err := worktree.Commit("base", &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("committing: %v", err)
	}
	if _, err := repo.CreateTag("v1", trunk, nil); err != nil {
		t.Fatalf("creating the tag: %v", err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "github", RefSpecs: []gitconfig.RefSpec{"refs/heads/master:refs/heads/master"}}); err != nil {
		t.Fatalf("pushing the old trunk: %v", err)
	}
	// mirrorRepository renames the trunk like this before the push.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), trunk)); err != nil {
		t.Fatalf("creating the new trunk: %v", err)
	}
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master")); err != nil {
		t.Fatalf("deleting the old trunk: %v", err)
	}

	p := &project{
		m: &Migrator{
			cfg:    &config.Config{TrimGithubBranches: true},
			logger: hclog.NewNullLogger(),
		},
		log:           hclog.NewNullLogger(),
		project:       &gogitlab.Project{DefaultBranch: "master"},
		repo:          repo,
		defaultBranch: "main",
		gitlabPath:    []string{"group", "project"},
		githubPath:    []string{"owner", "repo"},
	}

	// events records the GitHub API calls in their order.
	var events []string
	var branchesAtDefault []string
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("PATCH /repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		var repo gogithub.Repository
		if err := json.NewDecoder(r.Body).Decode(&repo); err != nil {
			t.Errorf("decoding the edited repository: %v", err)
		}
		events = append(events, "set default branch "+repo.GetDefaultBranch())
		branchesAtDefault = remoteBranches(t, remoteDir)
		// Like GitHub, the remote refuses to delete the branch its HEAD
		// points to, so the new default branch moves HEAD.
		remote, err := git.PlainOpen(remoteDir)
		if err != nil {
			t.Errorf("opening the remote repository: %v", err)
		} else if err := remote.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(repo.GetDefaultBranch()))); err != nil {
			t.Errorf("setting the remote HEAD: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"name":"repo"}`); err != nil {
			t.Errorf("writing the edited repository: %v", err)
		}
	})
	ghMux.HandleFunc("GET /repos/owner/repo/branches", func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "list branches")
		branches := make([]*gogithub.Branch, 0)
		for _, name := range remoteBranches(t, remoteDir) {
			branches = append(branches, &gogithub.Branch{Name: Pointer(name)})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(branches); err != nil {
			t.Errorf("writing the branches: %v", err)
		}
	})
	// The trim deletes the old trunk, so its open pull requests get the new
	// trunk as base first. There are none here.
	ghMux.HandleFunc("GET /repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "list open pull requests on "+r.URL.Query().Get("base"))
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("writing the pull requests: %v", err)
		}
	})
	useGitHubMux(t, p, ghMux)
	p.m.ghClient = clients.NewGitHubClient(p.m.gh, hclog.NewNullLogger())

	if err := p.pushToGitHub(context.Background(), "https://github.com/owner/repo", "master"); err != nil {
		t.Fatalf("pushToGitHub: %v", err)
	}

	if want := []string{"set default branch main", "list branches", "list open pull requests on master"}; !slices.Equal(events, want) {
		t.Fatalf("GitHub calls = %v, want %v", events, want)
	}
	if want := []string{"main", "master"}; !slices.Equal(branchesAtDefault, want) {
		t.Errorf("GitHub branches when the default branch was set = %v, want %v", branchesAtDefault, want)
	}
	if want := []string{"main"}; !slices.Equal(remoteBranches(t, remoteDir), want) {
		t.Errorf("GitHub branches after the push = %v, want %v", remoteBranches(t, remoteDir), want)
	}
	remote, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatalf("opening the remote repository: %v", err)
	}
	if _, err := remote.Tag("v1"); err != nil {
		t.Errorf("tag v1 on GitHub: %v", err)
	}
}
