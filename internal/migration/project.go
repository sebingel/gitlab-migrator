package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	plumbingcache "github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// project holds the state for a single project migration.
type project struct {
	m   *Migrator
	log hclog.Logger

	project       *gogitlab.Project
	repo          *git.Repository
	defaultBranch string
	gitlabPath    []string
	githubPath    []string
	storagePath   string
	result        ProjectResult
	state         *MigrationState // nil when -state-dir not set
}

func (m *Migrator) newProject(ctx context.Context, slugs []string) (*project, error) {
	var err error
	p := &project{m: m}
	p.log = m.logger.Named(slugs[0])

	p.gitlabPath, p.githubPath, err = ParseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %w", err)
	}

	p.log.Info("searching for GitLab project", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	p.project, _, err = m.gl.Projects.GetProject(slugs[0], nil, gogitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("retrieving project: %w", err)
	}

	if p.project == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	// -rename-master-to-main renames the default branch of the GitLab project,
	// whatever its name. Say so when that is neither "master" nor already "main".
	if m.cfg.RenameMasterToMain && p.project.DefaultBranch != "" && p.project.DefaultBranch != "master" && p.project.DefaultBranch != "main" {
		p.log.Warn(fmt.Sprintf("-rename-master-to-main: the default branch of the GitLab project is %q, not \"master\"; it will be renamed to \"main\"", p.project.DefaultBranch))
	}

	p.defaultBranch = "main"
	if m.cfg.RenameTrunkBranch != "" {
		p.defaultBranch = m.cfg.RenameTrunkBranch
	} else if !m.cfg.RenameMasterToMain && p.project.DefaultBranch != "" {
		p.defaultBranch = p.project.DefaultBranch
	}

	return p, nil
}

var pathSeparatorReplacer = strings.NewReplacer("/", "_", "\\", "_")

// sanitizePathSegment replaces the path separators in s, so that s can be part
// of a single directory name.
func sanitizePathSegment(s string) string {
	return pathSeparatorReplacer.Replace(s)
}

func (p *project) createGitStorage() (storage.Storer, error) {
	if p.m.cfg.StorageType == "filesystem" {
		// An empty StorageDir makes MkdirTemp use os.TempDir().
		// The group path of a subgroup project contains "/", which MkdirTemp
		// rejects in a pattern.
		pattern := fmt.Sprintf("gitlab-migrator-%s-%s-*", sanitizePathSegment(p.gitlabPath[0]), sanitizePathSegment(p.gitlabPath[1]))
		tempDir, err := os.MkdirTemp(p.m.cfg.StorageDir, pattern)
		if err != nil {
			return nil, fmt.Errorf("creating storage directory: %w", err)
		}

		p.storagePath = tempDir
		p.log.Debug("using filesystem storage", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "path", tempDir)

		gitDir := filepath.Join(tempDir, ".git")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			return nil, fmt.Errorf("creating .git directory: %w", err)
		}

		fs := osfs.New(gitDir)
		stor := filesystem.NewStorage(fs, plumbingcache.NewObjectLRUDefault())
		return stor, nil
	}

	p.log.Debug("using memory storage", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	return memory.NewStorage(), nil
}

func (p *project) cleanupStorage() {
	if p.storagePath != "" {
		p.log.Debug("cleaning up filesystem storage", "path", p.storagePath)
		if err := os.RemoveAll(p.storagePath); err != nil {
			p.log.Warn("failed to cleanup storage directory", "path", p.storagePath, "error", err)
		}
	}
}

var controlCharRegex = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func sanitizeDescription(s string) string {
	return controlCharRegex.ReplaceAllString(s, " ")
}

func (p *project) createRepo(ctx context.Context, homepage string, repoDeleted bool) error {
	if repoDeleted {
		p.log.Warn("recreating GitHub repository", "owner", p.githubPath[0], "repo", p.githubPath[1])
	} else {
		p.log.Debug("repository not found on GitHub, proceeding to create", "owner", p.githubPath[0], "repo", p.githubPath[1])
	}
	description := sanitizeDescription(p.project.Description)
	newRepo := gogithub.Repository{
		Name:          Pointer(p.githubPath[1]),
		Description:   &description,
		Homepage:      &homepage,
		DefaultBranch: &p.defaultBranch,
		Visibility:    Pointer(p.m.cfg.RepoVisibility),
		HasIssues:     Pointer(true),
		HasProjects:   Pointer(true),
		HasWiki:       Pointer(true),
	}
	if _, _, err := p.m.gh.Repositories.Create(ctx, p.githubPath[0], &newRepo); err != nil {
		return fmt.Errorf("creating github repo: %w", err)
	}
	return nil
}

func (p *project) pushErrHint(err error) string {
	hint := ""
	if err != nil && strings.Contains(err.Error(), "without 'workflow' scope") {
		hint = " (hint: add 'workflow' scope to your GitHub token to push workflow files)"
	}
	if p.m.cfg.NoForce && isNonFastForwardPushError(err) {
		hint = " (hint: remove -no-force if push is rejected due to conflicts)" + hint
	}
	return hint
}

// isNonFastForwardPushError reports whether a push failed because the remote
// ref has commits that the pushed ref does not have, so that only a force
// push can update it. go-git does not wrap git.ErrNonFastForwardUpdate for a
// push. Its own check before the push returns "non-fast-forward update:
// <ref>", and a rejection by the remote returns "command error on <ref>:
// non-fast-forward". A ref name cannot contain ":", so a ref name cannot
// cause a false match. When a tag on the remote differs from a local
// annotated tag, go-git returns "object not found" instead. That error can
// have other causes, so it gets no hint.
func isNonFastForwardPushError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "non-fast-forward update: ") ||
		(strings.HasPrefix(msg, "command error on ") && strings.HasSuffix(msg, ": non-fast-forward"))
}

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
var gitProgressLineRegex = regexp.MustCompile(`(?i)^(Compressing|Counting|Enumerating|Receiving|Resolving|Writing) (objects|deltas)\b`)
var prNumberRegex = regexp.MustCompile(`.+/([0-9]+)$`)

func cleanSidebandOutput(raw string, maxLen int) string {
	cleaned := ansiEscapeRegex.ReplaceAllString(raw, "")
	cleaned = strings.ReplaceAll(cleaned, "\x00", "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	lines := strings.Split(cleaned, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		line = strings.TrimPrefix(line, "remote: ")
		if gitProgressLineRegex.MatchString(strings.TrimSpace(line)) {
			continue
		}
		filtered = append(filtered, line)
	}
	result := strings.TrimSpace(strings.Join(filtered, "\n"))
	if maxLen > 0 && len(result) > maxLen {
		result = result[:maxLen] + "\n... (truncated)"
	}
	return result
}

func (p *project) pushWithSideband(ctx context.Context, opts *git.PushOptions) (string, error) {
	var buf bytes.Buffer
	opts.Progress = &buf
	err := p.repo.PushContext(ctx, opts)
	sideband := cleanSidebandOutput(buf.String(), 0)
	if err == nil && sideband != "" {
		p.log.Trace("push sideband output", "output", sideband)
	}
	return sideband, err
}

func formatPushError(msg, hint string, err error, sideband string) error {
	base := fmt.Sprintf("%s%s: %v", msg, hint, err)
	if sideband != "" {
		return fmt.Errorf("%s\n--- remote output ---\n%s", base, sideband)
	}
	return fmt.Errorf("%s", base)
}

func (p *project) migrate(ctx context.Context) (result ProjectResult, err error) {
	p.result = ProjectResult{
		GitLabGroup:      p.gitlabPath[0],
		GitLabProject:    p.gitlabPath[1],
		GitHubOwner:      p.githubPath[0],
		GitHubRepo:       p.githubPath[1],
		StartTime:        time.Now(),
		BranchesMigrated: make([]string, 0),
		MergeRequests:    make([]MergeRequestResult, 0),
	}
	// Every return, also an error return, reports the end time and the
	// branches mirrored so far. This deferred func runs last, after the
	// re-archive and the storage cleanup.
	defer func() {
		p.result.EndTime = time.Now()
		p.result.Duration = p.result.EndTime.Sub(p.result.StartTime)
		p.result.BranchCount = len(p.result.BranchesMigrated)
		result = p.result
	}()

	p.log.Debug("checking for existing repository on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1])
	githubRepo, _, err := p.m.gh.Repositories.Get(ctx, p.githubPath[0], p.githubPath[1])

	if err != nil && !isGitHubNotFound(err) {
		return p.result, fmt.Errorf("retrieving github repo: %w", err)
	}

	var wasArchived bool
	if err == nil && githubRepo != nil {
		wasArchived = githubRepo.GetArchived()
	}

	if wasArchived && p.m.cfg.UnarchiveArchivedRepos && !p.m.cfg.DeleteExistingRepos {
		p.log.Info("GitHub repo is archived, temporarily unarchiving for migration", "owner", p.githubPath[0], "repo", p.githubPath[1])
		rearchive := func() {
			archCtx, stop := detachedContext(ctx, rearchiveGracePeriod)
			defer stop()
			if archErr := p.setArchivedWithRetry(archCtx, true); archErr != nil {
				p.log.Warn("failed to re-archive GitHub repo after migration, manual re-archive required", "owner", p.githubPath[0], "repo", p.githubPath[1], "error", archErr)
			} else {
				p.log.Info("re-archived GitHub repo to restore original state", "owner", p.githubPath[0], "repo", p.githubPath[1])
			}
		}
		if unarchErr := p.setArchived(ctx, false); unarchErr != nil {
			// After Ctrl+C during the request, GitHub may have applied the
			// unarchive although the client only reports the cancel. Any other
			// error means GitHub did not unarchive, even when Ctrl+C came later.
			if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(unarchErr, ctxErr) {
				rearchive()
			}
			return p.result, fmt.Errorf("unarchiving github repo for migration: %w", unarchErr)
		}
		defer rearchive()
	}

	// The storage is created by mirrorRepository and is still needed by migrateMergeRequests.
	defer p.cleanupStorage()

	if p.m.cfg.PullRequestsOnly {
		if err != nil {
			return p.result, fmt.Errorf("GitHub repository %s/%s not found (-pull-requests-only requires the repository to already exist on GitHub)", p.githubPath[0], p.githubPath[1])
		}
		p.log.Info("pull-requests-only mode: skipping repository clone and push", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	} else if mirrorErr := p.mirrorRepository(ctx, err == nil, githubRepo.GetDefaultBranch()); mirrorErr != nil {
		return p.result, mirrorErr
	}

	if p.m.cfg.StateDir != "" && p.m.cfg.EnablePullRequests {
		if err := os.MkdirAll(p.m.cfg.StateDir, 0755); err != nil {
			return p.result, fmt.Errorf("creating state directory: %w", err)
		}
		statePath := filepath.Join(p.m.cfg.StateDir,
			sanitizeStateFileName(p.gitlabPath[0], p.gitlabPath[1], p.githubPath[0], p.githubPath[1])+".json")
		p.state, err = LoadOrCreate(statePath,
			p.gitlabPath[0]+"/"+p.gitlabPath[1],
			p.githubPath[0]+"/"+p.githubPath[1],
			p.log)
		if err != nil {
			return p.result, fmt.Errorf("loading migration state: %w", err)
		}
		total, success, _, skipped, _ := p.state.Summary()
		if total > 0 {
			p.log.Info("resuming from saved state", "total_tracked", total,
				"previously_successful", success, "previously_skipped", skipped,
				"state_file", statePath)
		} else {
			p.log.Debug("state persistence enabled", "state_file", statePath)
		}
	}

	var mrErr error
	if p.m.cfg.EnablePullRequests {
		var mrResults []MergeRequestResult
		mrResults, mrErr = p.migrateMergeRequests(ctx)
		p.result.MergeRequests = mrResults

		for _, mr := range mrResults {
			p.result.TotalMRs++
			switch mr.Status {
			case StatusSuccess:
				p.result.SuccessfulMRs++
			case StatusFailed:
				p.result.FailedMRs++
			case StatusSkipped:
				p.result.SkippedMRs++
			case StatusPartial:
				p.result.SuccessfulMRs++
			}
		}
	}

	// The report keeps the merge requests processed before an interrupt and
	// the branches mirrored before the failure.
	if mrErr != nil {
		return p.result, mrErr
	}

	if p.result.FailedMRs > 0 {
		if p.result.SuccessfulMRs > 0 {
			p.result.Status = StatusPartial
		} else {
			p.result.Status = StatusFailed
		}
	} else {
		p.result.Status = StatusSuccess
	}

	return p.result, nil
}

// mirrorRepository creates or updates the GitHub repository and mirror-pushes
// all branches and tags from GitLab. It leaves the local clone in p.repo for
// the merge request migration. githubDefaultBranch is the default branch of the
// GitHub repository before the run, empty when it does not exist yet.
func (p *project) mirrorRepository(ctx context.Context, repoExists bool, githubDefaultBranch string) error {
	var err error

	cloneUrl, parseErr := url.Parse(p.project.HTTPURLToRepo)
	if parseErr != nil {
		return fmt.Errorf("parsing clone URL: %v", parseErr)
	}

	p.log.Info("mirroring repository from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_org", p.githubPath[0], "github_repo", p.githubPath[1], "force", !p.m.cfg.NoForce)

	homepage := fmt.Sprintf("https://%s/%s/%s", p.m.cfg.GitlabDomain, p.gitlabPath[0], p.gitlabPath[1])

	if !repoExists {
		if err = p.createRepo(ctx, homepage, false); err != nil {
			return err
		}
	} else if p.m.cfg.DeleteExistingRepos {
		p.log.Warn("existing repository was found on GitHub, proceeding to delete", "owner", p.githubPath[0], "repo", p.githubPath[1])
		if _, err = p.m.gh.Repositories.Delete(ctx, p.githubPath[0], p.githubPath[1]); err != nil {
			return fmt.Errorf("deleting existing github repo: %w", err)
		}

		if err = p.createRepo(ctx, homepage, true); err != nil {
			return err
		}
	}

	p.log.Debug("updating repository settings", "owner", p.githubPath[0], "repo", p.githubPath[1])
	description := sanitizeDescription(p.project.Description)
	updateRepo := gogithub.Repository{
		Name:              Pointer(p.githubPath[1]),
		Description:       &description,
		Homepage:          &homepage,
		AllowAutoMerge:    Pointer(true),
		AllowMergeCommit:  Pointer(true),
		AllowRebaseMerge:  Pointer(true),
		AllowSquashMerge:  Pointer(true),
		AllowUpdateBranch: Pointer(true),
	}
	if _, _, err = p.m.gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return p.addArchivedHint(fmt.Errorf("updating github repo: %w", err))
	}

	cloneUrl.User = url.UserPassword("oauth2", p.m.cfg.GitlabToken)
	cloneUrlWithCredentials := cloneUrl.String()

	stor, err := p.createGitStorage()
	if err != nil {
		return fmt.Errorf("creating git storage: %w", err)
	}

	p.log.Debug("cloning repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", p.project.HTTPURLToRepo)
	// A nil worktree makes a bare clone. The pushes need only refs and objects,
	// so the default branch is not checked out into memory.
	p.repo, err = git.CloneContext(ctx, stor, nil, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		return fmt.Errorf("cloning gitlab repo: %w", err)
	}

	if p.defaultBranch != p.project.DefaultBranch {
		if gitlabTrunk, err := p.repo.Reference(plumbing.NewBranchReferenceName(p.project.DefaultBranch), false); err == nil {
			p.log.Info("renaming trunk branch prior to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())

			p.log.Debug("creating new trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())
			githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(p.defaultBranch), gitlabTrunk.Hash())
			if err = p.repo.Storer.SetReference(githubTrunk); err != nil {
				return fmt.Errorf("creating trunk branch: %w", err)
			}

			p.log.Debug("deleting old trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "sha", gitlabTrunk.Hash())
			if err = p.repo.Storer.RemoveReference(gitlabTrunk.Name()); err != nil {
				return fmt.Errorf("deleting old trunk branch: %w", err)
			}
		}
	}

	githubUrl := fmt.Sprintf("https://%s/%s/%s", p.m.cfg.GithubDomain, p.githubPath[0], p.githubPath[1])
	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", p.m.cfg.GithubUser, p.m.cfg.GithubToken, p.m.cfg.GithubDomain, p.githubPath[0], p.githubPath[1])

	p.log.Debug("adding remote for GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	if _, err = p.repo.CreateRemote(&gitconfig.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return fmt.Errorf("adding github remote: %w", err)
	}

	return p.pushToGitHub(ctx, githubUrl, githubDefaultBranch)
}

// pushToGitHub pushes the branches and tags of p.repo to its remote "github",
// then sets the default branch of the GitHub repository and, with
// -trim-branches-on-github, deletes the GitHub branches that GitLab does not
// have (see updateGithubBranches). githubUrl is the repository URL without
// credentials, for the log. githubDefaultBranch is the default branch of the
// GitHub repository before the run, empty when it did not exist yet.
func (p *project) pushToGitHub(ctx context.Context, githubUrl, githubDefaultBranch string) error {
	p.log.Debug("determining branches to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	branches, err := p.repo.Branches()
	if err != nil {
		return fmt.Errorf("retrieving branches: %w", err)
	}

	refSpecs := make([]gitconfig.RefSpec, 0)
	if err = branches.ForEach(func(ref *plumbing.Reference) error {
		branchName := ref.Name().Short()
		p.result.BranchesMigrated = append(p.result.BranchesMigrated, branchName)
		refSpecs = append(refSpecs, gitconfig.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref.Name())))
		return nil
	}); err != nil {
		return fmt.Errorf("parsing branches: %w", err)
	}

	batches := ChunkRefSpecs(refSpecs, p.m.cfg.PushBatchSize)
	pushMode := "force-pushing"
	if p.m.cfg.NoForce {
		pushMode = "pushing"
	}
	p.log.Debug(pushMode+" branches to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "total_branches", len(refSpecs), "batches", len(batches), "batch_size", p.m.cfg.PushBatchSize)

	for batchNum, batch := range batches {
		p.log.Debug("pushing branch batch", "name", p.gitlabPath[1], "batch", batchNum+1, "total_batches", len(batches), "branches_in_batch", len(batch))

		opts := &git.PushOptions{
			RemoteName: "github",
			Force:      !p.m.cfg.NoForce,
			RefSpecs:   batch,
		}
		sideband, err := p.pushWithSideband(ctx, opts)
		if err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				p.log.Debug("batch already up-to-date", "batch", batchNum+1)
			} else {
				msg := fmt.Sprintf("pushing branch batch %d/%d to github", batchNum+1, len(batches))
				return formatPushError(msg, p.pushErrHint(err), err, sideband)
			}
		}
	}

	p.log.Debug(pushMode+" tags to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	tagOpts := &git.PushOptions{
		RemoteName: "github",
		Force:      !p.m.cfg.NoForce,
		RefSpecs:   []gitconfig.RefSpec{"refs/tags/*:refs/tags/*"},
	}
	tagSideband, err := p.pushWithSideband(ctx, tagOpts)
	if err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			p.log.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		} else {
			return formatPushError("pushing tags to github repo", p.pushErrHint(err), err, tagSideband)
		}
	}

	return p.updateGithubBranches(ctx, githubUrl, githubDefaultBranch)
}

// updateGithubBranches is called after the branches and tags are pushed to GitHub. It
// makes the GitHub trunk the default branch of the GitHub repository and, with
// -trim-branches-on-github, deletes the branches on GitHub that are not in the
// GitLab repository. The default branch comes first: after a trunk rename the
// trim deletes the old trunk, which an earlier run without the rename made the
// default branch, and GitHub refuses to delete the default branch.
func (p *project) updateGithubBranches(ctx context.Context, githubUrl, githubDefaultBranch string) error {
	p.log.Debug("setting default repository branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "branch_name", p.defaultBranch)
	updateRepoDefault := gogithub.Repository{
		DefaultBranch: &p.defaultBranch,
	}
	if _, _, err := p.m.gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepoDefault); err != nil {
		return fmt.Errorf("setting default branch: %w", err)
	}

	if !p.m.cfg.TrimGithubBranches {
		return nil
	}

	p.log.Debug("determining old branches to trim on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	refSpecsToDelete := make([]gitconfig.RefSpec, 0)
	branchesToDelete := make([]string, 0)
	githubBranches, err := p.m.ghClient.GetBranches(ctx, p.githubPath[0], p.githubPath[1])
	if err != nil {
		return fmt.Errorf("listing branches from GitHub: %w", err)
	}
	for _, githubBranch := range githubBranches {
		// Dereference Name on purpose: a branch without a name must fail loudly,
		// not become the refspec ":refs/heads/" in the delete batch.
		if !slices.Contains(p.result.BranchesMigrated, *githubBranch.Name) {
			branchesToDelete = append(branchesToDelete, *githubBranch.Name)
			refSpecsToDelete = append(refSpecsToDelete, gitconfig.RefSpec(fmt.Sprintf(":refs/heads/%s", *githubBranch.Name)))
		}
	}

	if err := p.retargetPullRequestsBeforeTrim(ctx, branchesToDelete, githubDefaultBranch); err != nil {
		return err
	}

	batches := ChunkRefSpecs(refSpecsToDelete, p.m.cfg.PushBatchSize)
	p.log.Debug("trimming old branches on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "total_branches", len(refSpecsToDelete), "batches", len(batches))

	for batchNum, batch := range batches {
		p.log.Debug("trimming branch batch", "name", p.gitlabPath[1], "batch", batchNum+1, "total_batches", len(batches), "branches_in_batch", len(batch))

		trimOpts := &git.PushOptions{
			RemoteName: "github",
			Force:      true,
			RefSpecs:   batch,
		}
		sideband, err := p.pushWithSideband(ctx, trimOpts)
		if err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				p.log.Debug("batch already up-to-date", "batch", batchNum+1)
			} else {
				return formatPushError(fmt.Sprintf("trimming branch batch %d/%d", batchNum+1, len(batches)), "", err, sideband)
			}
		}
	}
	return nil
}

// mergeRequestListOptions returns the options for listing the merge requests
// of a project, oldest first. With maxAgeDays above 0 only the merge requests
// created in the last maxAgeDays days are listed (-merge-requests-max-age).
// The migration and the report both use it, so the report counts the merge
// requests that a migration processes.
func mergeRequestListOptions(maxAgeDays int) *gogitlab.ListProjectMergeRequestsOptions {
	opts := &gogitlab.ListProjectMergeRequestsOptions{
		ListOptions: gogitlab.ListOptions{PerPage: 100},
		OrderBy:     Pointer("created_at"),
		Sort:        Pointer("asc"),
	}

	if maxAgeDays > 0 {
		opts.CreatedAfter = Pointer(time.Now().AddDate(0, 0, -maxAgeDays))
	}

	return opts
}

// migrateMergeRequests migrates the merge requests of the project. It returns
// an error when the merge requests cannot be listed (then none of them were
// migrated and the results are nil) or when ctx is canceled while they are
// processed (then the results hold the processed ones). The failures of
// single merge requests are in the results.
func (p *project) migrateMergeRequests(ctx context.Context) ([]MergeRequestResult, error) {
	var mergeRequests []*gogitlab.BasicMergeRequest

	opts := mergeRequestListOptions(p.m.cfg.MergeRequestsAge)

	p.log.Debug("retrieving GitLab merge requests", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID)
	for {
		result, resp, err := p.m.gl.MergeRequests.ListProjectMergeRequests(p.project.ID, opts, gogitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("retrieving gitlab merge requests: %w", err)
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	results := make([]MergeRequestResult, 0, len(mergeRequests))
	p.log.Info("migrating merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", len(mergeRequests))

	var interrupted error
	for _, mergeRequest := range mergeRequests {
		if err := ctx.Err(); err != nil {
			p.log.Warn("migration interrupted, stopping merge request processing", "remaining", len(mergeRequests)-len(results))
			interrupted = fmt.Errorf("migration interrupted after %d of %d merge requests: %w", len(results), len(mergeRequests), err)
			break
		}

		if mergeRequest == nil {
			continue
		}

		if p.state != nil && p.state.ShouldSkip(mergeRequest.IID) {
			prev := p.state.GetState(mergeRequest.IID)
			if prev == nil {
				p.log.Warn("state inconsistency: ShouldSkip=true but GetState=nil, reprocessing", "mr_iid", mergeRequest.IID)
			} else {
				p.log.Debug("skipping MR from saved state", "mr_iid", mergeRequest.IID, "status", prev.Status)
				status := StatusSuccess
				if prev.Status == MRStateSkipped {
					status = StatusSkipped
				}
				mrResult := MergeRequestResult{
					GitLabMRID:     mergeRequest.IID,
					GitLabMRTitle:  mergeRequest.Title,
					GitLabState:    mergeRequest.State,
					GitHubPRNumber: prev.GitHubPRNum,
					Status:         status,
					SkipReason:     prev.SkipReason,
				}
				// The state file is not changed for a failure here: it keeps
				// the saved result, so the next run tries the base again.
				if err := p.retargetSavedPullRequest(ctx, mergeRequest, prev.GitHubPRNum); err != nil {
					err = p.addArchivedHint(err)
					p.log.Error("changing base branch of migrated pull request", "merge_request_id", mergeRequest.IID, "error", err)
					mrResult.Status = StatusFailed
					mrResult.Error = err.Error()
				}
				results = append(results, mrResult)
				continue
			}
		}

		if p.m.cfg.SkipOpenMergeRequests && strings.EqualFold(mergeRequest.State, "opened") {
			// The state file does not get this skip. It depends on the flags of
			// this run, and the MR can be merged or closed before the next run.
			results = append(results, MergeRequestResult{
				GitLabMRID:    mergeRequest.IID,
				GitLabMRTitle: mergeRequest.Title,
				GitLabState:   mergeRequest.State,
				Status:        StatusSkipped,
				SkipReason:    skipReasonOpenMergeRequest,
			})
			continue
		}

		mrResult, err := p.migrateMergeRequest(ctx, mergeRequest)
		if err != nil {
			err = p.addArchivedHint(err)
			p.log.Error("migrating merge request", "merge_request_id", mergeRequest.IID, "error", err)
			mrResult.Status = StatusFailed
			mrResult.Error = err.Error()
		}
		results = append(results, mrResult)

		if p.state != nil {
			switch mrResult.Status {
			case StatusSuccess:
				p.state.RecordSuccess(mergeRequest.IID, mrResult.GitHubPRNumber)
			case StatusFailed:
				p.state.RecordFailure(mergeRequest.IID, mrResult.Error)
			case StatusSkipped:
				p.state.RecordSkipped(mergeRequest.IID, mrResult.SkipReason)
			case StatusPartial:
				p.state.RecordPartial(mergeRequest.IID, mrResult.GitHubPRNumber, mrResult.Error)
			}
			if flushErr := p.state.Flush(); flushErr != nil {
				p.log.Error("failed to persist migration state", "error", flushErr)
			}
		}
	}

	// The check at the top of the loop does not see a cancel while the last
	// merge request was processed. That merge request can still count as
	// migrated, for example as partial when only a comment failed.
	if err := ctx.Err(); err != nil && interrupted == nil {
		p.log.Warn("migration interrupted while processing the merge requests")
		interrupted = fmt.Errorf("migration interrupted while processing the merge requests: %w", err)
	}

	var successCount, failureCount, skippedCount int
	for _, result := range results {
		switch result.Status {
		case StatusSuccess, StatusPartial:
			successCount++
		case StatusFailed:
			failureCount++
		case StatusSkipped:
			skippedCount++
		}
	}

	p.log.Info("migrated merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "successful", successCount, "failed", failureCount, "skipped", skippedCount)

	return results, interrupted
}

func (p *project) migrateMergeRequest(ctx context.Context, mergeRequest *gogitlab.BasicMergeRequest) (finalResult MergeRequestResult, finalErr error) {
	result := MergeRequestResult{
		GitLabMRID:    mergeRequest.IID,
		GitLabMRTitle: mergeRequest.Title,
		GitLabState:   mergeRequest.State,
		SourceBranch:  mergeRequest.SourceBranch,
		TargetBranch:  mergeRequest.TargetBranch,
		Comments:      make([]CommentResult, 0),
	}

	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("preparing to list pull requests: %w", err)
	}

	sourceBranchForClosedMergeRequest := fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)
	targetBranchForClosedMergeRequest := fmt.Sprintf("migration-target-%d/%s", mergeRequest.IID, mergeRequest.TargetBranch)

	var pullRequest *gogithub.PullRequest

	// The deferred cleanup below deletes the temporary branches of a closed
	// merge request when its pull request exists, or when GitHub refused the
	// pull request because the branches have no commits between them: then no
	// pull request will ever use them. After other errors the branches stay.
	// A pull request that an earlier run created counts too: that run can have
	// stopped before it deleted the branches (issue #142). With
	// -pull-requests-only, createTempBranchesViaAPI deletes the branches it
	// created itself when it fails or skips the merge request, because no pull
	// request exists then.
	noCommitsBetween := false
	keepTempBranches := func() bool {
		return pullRequest == nil && !noCommitsBetween
	}

	p.log.Debug("searching for any existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID, "state", mergeRequest.State, "source_branch", mergeRequest.SourceBranch)
	sourceBranches := []string{mergeRequest.SourceBranch, sourceBranchForClosedMergeRequest}
	branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
	query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", p.githubPath[0], p.githubPath[1], branchQuery)
	searchResult, err := p.m.ghClient.GetSearchResults(ctx, query)
	if err != nil {
		if isSearchSyntaxError(err) {
			p.log.Warn("search query failed due to special characters in branch name - falling back to list API",
				"source_branch", mergeRequest.SourceBranch, "error", err)
			pullRequest, err = p.findExistingPRByList(ctx, mergeRequest)
			if err != nil {
				return result, fmt.Errorf("listing pull requests (fallback): %w", err)
			}
			if pullRequest != nil {
				result.GitHubPRNumber = pullRequest.Number
			}
		} else {
			return result, fmt.Errorf("listing pull requests: %w", err)
		}
	}

	if searchResult != nil {
		for _, issue := range searchResult.Issues {
			if issue == nil {
				continue
			}

			if err := ctx.Err(); err != nil {
				return result, fmt.Errorf("preparing to retrieve pull request: %w", err)
			}

			if issue.IsPullRequest() {
				// The number of a pull request is the number of its issue. It
				// is used when GitHub sends no pull request URL.
				prNumber := issue.GetNumber()
				if rawURL := issue.GetPullRequestLinks().GetURL(); rawURL != "" {
					prUrl, err := url.Parse(rawURL)
					if err != nil {
						return result, fmt.Errorf("parsing pull request url: %w", err)
					}
					m := prNumberRegex.FindStringSubmatch(prUrl.Path)
					if len(m) != 2 {
						continue
					}
					prNumber, _ = strconv.Atoi(m[1])
				}
				if prNumber == 0 {
					p.log.Debug("ignoring search result without pull request number", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
					continue
				}

				pr, err := p.m.ghClient.GetPullRequest(ctx, p.githubPath[0], p.githubPath[1], prNumber)
				if err != nil {
					return result, fmt.Errorf("retrieving pull request: %w", err)
				}

				if bodyMatchesMergeRequest(pr.GetBody(), mergeRequest.IID) {
					p.log.Debug("found existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pr.GetNumber())
					pullRequest = pr
					result.GitHubPRNumber = pullRequest.Number
					break
				}
			}
		}
	}

	if strings.EqualFold(mergeRequest.State, "opened") {
		branchRef := plumbing.NewBranchReferenceName(mergeRequest.SourceBranch)
		p.log.Debug("checking for source branch in local mirror", "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch, "ref_name", branchRef.String())

		if ref, err := p.repo.Reference(branchRef, false); err != nil {
			p.log.Debug("branch lookup failed", "merge_request_id", mergeRequest.IID, "error", err, "error_type", fmt.Sprintf("%T", err))
			if errors.Is(err, plumbing.ErrReferenceNotFound) && p.m.cfg.SkipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as source branch does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)
				result.Status = StatusSkipped
				result.SkipReason = "source branch does not exist"
				return result, nil
			} else {
				return result, fmt.Errorf("checking source branch for merge request: %w", err)
			}
		} else {
			p.log.Debug("branch found successfully", "merge_request_id", mergeRequest.IID, "ref", ref.Name().String(), "hash", ref.Hash())
		}
	}

	if !strings.EqualFold(mergeRequest.State, "opened") {
		// A pull request that the search found was created by an earlier run.
		// Its temporary branches are deleted only when GitHub lists them, so a
		// rerun sends no deletion for the pull requests whose branches are gone
		// already, or that were migrated while their merge request was open and
		// never had temporary branches. The list is read once per repository and
		// then cached, so it can miss the branches that this run created: a pull
		// request that this run created does not use it.
		//
		// When the run is stopped before the branches are deleted, they stay. A
		// success or a skip is never migrated again with -state-dir, so a
		// success becomes partial, and the skip of "no commits between" becomes
		// a failure (it has no pull request, so it is not counted as migrated):
		// the next run migrates the merge request again and deletes the branches
		// then. The defer changes the named result for that, because it runs
		// after the return statement.
		foundPullRequest := pullRequest != nil
		defer func() {
			if keepTempBranches() {
				return
			}
			stopped := p.deleteTempBranches(ctx, pullRequest.GetNumber(), foundPullRequest, sourceBranchForClosedMergeRequest, targetBranchForClosedMergeRequest)
			if !stopped || finalErr != nil {
				return
			}
			const stoppedError = "the run was stopped before the temporary branches were deleted"
			switch finalResult.Status {
			case StatusSuccess, StatusPartial:
				finalResult.Status = StatusPartial
				if finalResult.Error == "" {
					finalResult.Error = stoppedError
				}
			case StatusSkipped:
				// The report shows a skip reason before the error, so the
				// old reason would hide the stop.
				finalResult.Status = StatusFailed
				finalResult.SkipReason = ""
				finalResult.Error = stoppedError
			}
		}()
	}

	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		p.log.Trace("searching for existing branch for closed/merged merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

		mergeRequest.SourceBranch = sourceBranchForClosedMergeRequest
		mergeRequest.TargetBranch = targetBranchForClosedMergeRequest

		p.log.Trace("retrieving commits for merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
		mergeRequestCommits, err := p.listMergeRequestCommits(ctx, mergeRequest.IID)
		if err != nil {
			return result, fmt.Errorf("retrieving merge request commits: %w", err)
		}

		if len(mergeRequestCommits) == 0 {
			result.Status = StatusSkipped
			result.SkipReason = "merge request has no commits"
			return result, nil
		}

		if mergeRequestCommits[0] == nil {
			return result, fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
		}
		if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
			return result, fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
		}

		if p.m.cfg.PullRequestsOnly {
			skipped, err := p.createTempBranchesViaAPI(ctx, mergeRequest, mergeRequestCommits, &result)
			if err != nil {
				return result, err
			}
			if skipped {
				return result, nil
			}
		} else {
			startHash := plumbing.NewHash(mergeRequestCommits[0].ID)
			endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
			fetchErr := p.fetchMissingCommits(ctx, mergeRequest.IID, endHash, startHash)
			if fetchErr != nil {
				// go-git does not always wrap the stop in its error, for
				// example not for the download of the pack, so ctx decides.
				// A stop is no reason to skip the merge request.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, fmt.Errorf("fetching the commits of the merge request from GitLab: %w: %w", ctxErr, fetchErr)
				}
				// A network error, a server error or a refused login can be
				// gone in the next run, so it fails the merge request even with
				// -skip-invalid-merge-requests: with -state-dir a skip is
				// never migrated again.
				if isTransientFetchError(fetchErr) {
					return result, fmt.Errorf("fetching the commits of the merge request from GitLab failed, a later run can try again: %w", fetchErr)
				}
				p.log.Warn("could not fetch the commits of the merge request that the clone does not have", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "error", fetchErr)
			}

			p.log.Trace("inspecting start commit", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
			startCommit, err := object.GetCommit(p.repo.Storer, startHash)
			if err != nil {
				if p.m.cfg.SkipInvalidMergeRequests {
					p.log.Info("skipping invalid merge request as start commit does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "missing_commit", mergeRequestCommits[0].ShortID, "error", err)
					result.Status = StatusSkipped
					result.SkipReason = missingCommitSkipReason("start commit", err, fetchErr)
					return result, nil
				}
				return result, fmt.Errorf("loading start commit %s: %w%s", mergeRequestCommits[0].ShortID, err, missingCommitHint(err, fetchErr))
			}

			if startCommit.NumParents() == 0 {
				if p.m.cfg.SkipInvalidMergeRequests {
					p.log.Info("skipping invalid merge request as start commit has no parents", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", startCommit.Hash)
					result.Status = StatusSkipped
					result.SkipReason = "start commit has no parents (orphaned)"
					return result, nil
				}

				return result, fmt.Errorf("start commit %s for merge request %d has no parents", mergeRequestCommits[0].ShortID, mergeRequest.IID)
			} else {
				var startCommitParent *object.Commit
				for i := 0; i < startCommit.NumParents(); i++ {
					p.log.Trace("inspecting start commit parent", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "parent_index", i, "sha", mergeRequestCommits[0].ShortID)
					startCommitParent, err = startCommit.Parent(i)
					if err != nil {
						p.log.Error("loading parent commit", "index", i, "error", err)
						continue
					}
					break
				}

				if startCommitParent == nil {
					return result, fmt.Errorf("identifying suitable parent of start commit %s for merge request %d", mergeRequestCommits[0].ShortID, mergeRequest.IID)
				}

				p.log.Trace("creating target branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.TargetBranch, "sha", startCommitParent.Hash)
				if err = p.createLocalBranch(plumbing.NewBranchReferenceName(mergeRequest.TargetBranch), startCommitParent.Hash); err != nil {
					return result, fmt.Errorf("creating temporary target branch: %w", err)
				}
			}

			p.log.Trace("creating source branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)

			if _, err = object.GetCommit(p.repo.Storer, endHash); err != nil {
				if p.m.cfg.SkipInvalidMergeRequests {
					p.log.Info("skipping invalid merge request as end commit does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "missing_commit", mergeRequestCommits[len(mergeRequestCommits)-1].ShortID, "error", err)
					result.Status = StatusSkipped
					result.SkipReason = missingCommitSkipReason("end commit", err, fetchErr)
					return result, nil
				}
				return result, fmt.Errorf("loading end commit %s: %w%s", mergeRequestCommits[len(mergeRequestCommits)-1].ShortID, err, missingCommitHint(err, fetchErr))
			}

			if err = p.createLocalBranch(plumbing.NewBranchReferenceName(mergeRequest.SourceBranch), endHash); err != nil {
				return result, fmt.Errorf("creating temporary source branch: %w", err)
			}

			p.log.Debug("pushing branches for merged/closed merge request", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			mrPushOpts := &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []gitconfig.RefSpec{
					gitconfig.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
					gitconfig.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
				},
				Force: !p.m.cfg.NoForce,
			}
			mrSideband, err := p.pushWithSideband(ctx, mrPushOpts)
			if err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					p.log.Trace("branch already exists and is up-to-date on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				} else {
					return result, formatPushError("pushing temporary branches to github", p.pushErrHint(err), err, mrSideband)
				}
			}
		}
	}

	if p.defaultBranch != p.project.DefaultBranch && mergeRequest.TargetBranch == p.project.DefaultBranch {
		p.log.Trace("changing target trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "old_trunk", p.project.DefaultBranch, "new_trunk", p.defaultBranch)
		mergeRequest.TargetBranch = p.defaultBranch
	}

	githubAuthorName := "Unknown Author"
	if mergeRequest.Author != nil {
		author, err := p.m.glClient.GetUser(ctx, mergeRequest.Author.Username)
		if err != nil {
			return result, fmt.Errorf("retrieving gitlab user: %w", err)
		}
		githubAuthorName = githubMention(author, mergeRequest.Author.Name)
	}

	originalState := ""
	if !strings.EqualFold(mergeRequest.State, "opened") {
		originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
	}

	p.log.Debug("determining merge request approvers", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	approvers := make([]string, 0)
	awards, err := p.listMergeRequestAwardEmoji(ctx, mergeRequest.IID)
	if err != nil {
		p.log.Error("listing merge request awards", "error", err)
	} else {
		for _, award := range awards {
			if award.Name == "thumbsup" {
				approverUser, err := p.m.glClient.GetUser(ctx, award.User.Username)
				if err != nil {
					p.log.Error("retrieving gitlab user for approver", "username", award.User.Username, "error", err)
					continue
				}
				approvers = append(approvers, githubMention(approverUser, award.User.Name))
			}
		}
	}

	description := mergeRequest.Description
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	slices.Sort(approvers)
	approval := strings.Join(approvers, ", ")
	if approval == "" {
		approval = "_No approvers_"
	}

	closeDate := ""
	if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", mergeRequest.ClosedAt.Format(config.DateFormat))
	} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.Format(config.DateFormat))
	}

	mergeRequestTitle := shortenTitle(mergeRequest.Title)

	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | [%[4]s/%[5]s](https://%[10]s/%[4]s/%[5]s) |
> | **GitLab Merge Request** | [%[11]s](https://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **GitLab MR Number** | [%[2]d](https://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **Date Originally Opened** | %[6]s |%[7]s
> | **Approved on GitLab by** | %[8]s |
> |      |      |
>
%[9]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, p.gitlabPath[0], p.gitlabPath[1], formatDate(mergeRequest.CreatedAt), closeDate, approval, originalState, p.m.cfg.GitlabDomain, mergeRequestTitle)

	created := false
	if pullRequest == nil {
		p.log.Info("creating pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		newPullRequest := gogithub.NewPullRequest{
			Title:               &mergeRequest.Title,
			Head:                &mergeRequest.SourceBranch,
			Base:                &mergeRequest.TargetBranch,
			Body:                &body,
			MaintainerCanModify: Pointer(true),
			Draft:               &mergeRequest.Draft,
		}
		// Closure writes to outer pullRequest — on retry, only the last successful value is used.
		// retryOnNotFound passes through non-404 errors (e.g. 422 "already exists"),
		// which are handled by the error checks below.
		err = p.retryOnNotFound(ctx, "creating pull request", func() error {
			var createErr error
			pullRequest, _, createErr = p.m.gh.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest)
			return createErr
		})
		if err != nil {
			if strings.Contains(err.Error(), "No commits between") {
				p.log.Debug("skipping merge request as the change is already present in trunk branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
				noCommitsBetween = true
				result.Status = StatusSkipped
				result.SkipReason = fmt.Sprintf("branch '%s' has no new commits relative to '%s'; changes are already present in the target branch", mergeRequest.SourceBranch, mergeRequest.TargetBranch)
				return result, nil
			}
			// 422 "already exists" → search index lag, find PR via List API
			if isAlreadyExistsPRError(err) {
				p.log.Info("PR already exists (search index lag) - looking up via list API",
					"owner", p.githubPath[0], "repo", p.githubPath[1],
					"merge_request_id", mergeRequest.IID)
				pullRequest, err = p.findExistingPRByList(ctx, mergeRequest)
				if err != nil {
					return result, fmt.Errorf("finding existing PR after create conflict: %w", err)
				}
				if pullRequest == nil {
					return result, fmt.Errorf("creating pull request: PR already exists but could not be found via list API (MR !%d)", mergeRequest.IID)
				}
				// pullRequest is set → falls through into update path
			} else {
				return result, fmt.Errorf("creating pull request: %w", err)
			}
		} else {
			created = true
		}

		result.GitHubPRNumber = pullRequest.Number
	}

	if created {
		if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
			p.log.Debug("closing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.State = Pointer("closed")
			pullRequest, err = p.editPullRequest(ctx, "closing pull request", pullRequest.GetNumber(), pullRequest)
			if err != nil {
				return result, fmt.Errorf("updating pull request: %w", err)
			}
		}
	} else if pullRequest != nil {
		result.GitHubPRNumber = pullRequest.Number

		var newState *string
		switch mergeRequest.State {
		case "opened":
			newState = Pointer("open")
		case "closed", "merged":
			newState = Pointer("closed")
		}

		if pullRequest.State != nil && newState != nil && *pullRequest.State != *newState {
			editReq := &gogithub.PullRequest{
				Number: pullRequest.Number,
				State:  newState,
			}
			pullRequest, err = p.editPullRequest(ctx, "updating pull request state", pullRequest.GetNumber(), editReq)
			if err != nil {
				return result, fmt.Errorf("updating pull request state: %w", err)
			}
		}

		// The base branch follows the target branch of the merge request, for
		// example the new trunk after -rename-master-to-main or
		// -rename-trunk-branch. GitHub refuses to change the base branch of a
		// closed pull request, so only an open pull request gets a new base. It
		// is checked after the state change above, so a pull request that was
		// just reopened gets it too. A closed pull request keeps its base: for a
		// closed or merged merge request that is the temporary target branch
		// from the time it was migrated.
		var newBase *string
		if pullRequest.GetState() == "open" && pullRequest.GetBase().GetRef() != mergeRequest.TargetBranch {
			newBase = &mergeRequest.TargetBranch
		}

		if (newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState)) ||
			(pullRequest.Title == nil || *pullRequest.Title != mergeRequest.Title) ||
			(pullRequest.Body == nil || *pullRequest.Body != body) ||
			(pullRequest.Draft == nil || *pullRequest.Draft != mergeRequest.Draft) ||
			newBase != nil {
			p.log.Info("updating pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.Title = &mergeRequest.Title
			pullRequest.Body = &body
			pullRequest.Draft = &mergeRequest.Draft
			pullRequest.MaintainerCanModify = nil
			if newBase != nil {
				p.log.Info("changing base branch of pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "old_base", pullRequest.GetBase().GetRef(), "new_base", *newBase)
				// A new branch value instead of a change in place: the cache
				// of pull requests holds shallow copies that share the old one.
				pullRequest.Base = &gogithub.PullRequestBranch{Ref: newBase}
			}

			pullRequest, err = p.editPullRequest(ctx, "updating pull request", pullRequest.GetNumber(), pullRequest)
			if err != nil {
				return result, fmt.Errorf("updating pull request: %w", err)
			}
		} else {
			p.log.Trace("existing pull request is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	p.log.Debug("retrieving GitLab merge request comments", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	comments, err := p.listMergeRequestNotes(ctx, mergeRequest.IID)
	if err != nil {
		return result, fmt.Errorf("listing merge request notes: %w", err)
	}

	result.TotalComments = len(comments)

	p.log.Debug("retrieving GitHub pull request comments", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
	prComments, _, err := p.m.gh.Issues.ListComments(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &gogithub.IssueListCommentsOptions{Sort: Pointer("created"), Direction: Pointer("asc")})
	if err != nil {
		p.log.Error("listing pull request comments", "error", err)
	} else {
		p.log.Info("migrating merge request comments from GitLab to GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(comments))

		p.migrateComments(ctx, pullRequest, comments, prComments, &result)
	}

	// A run that was stopped by now keeps the temporary branches of a closed
	// merge request: the deferred cleanup makes the result partial then.
	if result.FailedComments > 0 {
		result.Status = StatusPartial
	} else {
		result.Status = StatusSuccess
	}

	return result, nil
}

// migrateComments writes the GitLab notes of a merge request as comments of its
// pull request. prComments are the comments the pull request has already. A
// note whose comment exists is updated when its text changed, any other note
// gets a new comment. Every note that is not skipped adds exactly one entry to
// result.Comments. Nil notes, system notes and notes without an author username
// are skipped.
func (p *project) migrateComments(ctx context.Context, pullRequest *gogithub.PullRequest, comments []*gogitlab.Note, prComments []*gogithub.IssueComment, result *MergeRequestResult) {
	for _, comment := range comments {
		if comment == nil || comment.System {
			continue
		}

		if comment.Author.Username == "" {
			p.log.Warn("skipping comment with unknown author", "comment_id", comment.ID)
			continue
		}

		commentResult := CommentResult{
			GitLabNoteID:   comment.ID,
			AuthorUsername: comment.Author.Username,
		}
		if comment.CreatedAt != nil {
			commentResult.CreatedAt = *comment.CreatedAt
		}

		commentAuthor, err := p.m.glClient.GetUser(ctx, comment.Author.Username)
		if err != nil {
			commentResult.Status = StatusFailed
			commentResult.Error = fmt.Sprintf("retrieving gitlab user: %v", err)
			result.Comments = append(result.Comments, commentResult)
			result.FailedComments++
			p.log.Error("retrieving gitlab user for comment", "comment_id", comment.ID, "error", err)
			continue
		}
		githubCommentAuthorName := githubMention(commentAuthor, comment.Author.Name)

		commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Note ID** | %[2]d |
> | **Date Originally Created** | %[3]s |
> |      |      |
>

%[5]s

%[4]s`, githubCommentAuthorName, comment.ID, formatDate(comment.CreatedAt), comment.Body, commentTextHeading)

		existingComment := findMigratedComment(prComments, comment.ID)
		if existingComment != nil {
			if existingComment.Body == nil || *existingComment.Body != commentBody {
				p.log.Debug("updating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", existingComment.GetID())
				existingComment.Body = &commentBody
				if _, _, err = p.m.gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], existingComment.GetID(), existingComment); err != nil {
					err = p.addArchivedHint(err)
					commentResult.Status = StatusFailed
					commentResult.Error = fmt.Sprintf("updating comment: %v", err)
					result.Comments = append(result.Comments, commentResult)
					result.FailedComments++
					p.log.Error("updating pull request comment", "comment_id", comment.ID, "error", err)
					continue
				}
			} else {
				p.log.Trace("existing pull request comment is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", existingComment.GetID())
			}
			commentResult.Status = StatusSuccess
			commentResult.GitHubCommentID = Pointer(existingComment.GetID())
			result.MigratedComments++
		} else {
			p.log.Debug("creating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
			newComment := gogithub.IssueComment{
				Body: &commentBody,
			}
			createdComment, _, err := p.m.gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment)
			if err != nil {
				err = p.addArchivedHint(err)
				commentResult.Status = StatusFailed
				commentResult.Error = fmt.Sprintf("creating comment: %v", err)
				result.Comments = append(result.Comments, commentResult)
				result.FailedComments++
				p.log.Error("creating pull request comment", "comment_id", comment.ID, "error", err)
				continue
			}
			commentResult.Status = StatusSuccess
			commentResult.GitHubCommentID = createdComment.ID
			result.MigratedComments++
		}

		result.Comments = append(result.Comments, commentResult)
	}
}

// listMergeRequestCommits returns all commits of the merge request, oldest
// first. GitLab lists them in git order, newest first, and has no option to
// change that, so it reads every page and reverses the list. GitLab itself
// takes the last commit of its list as the first commit of the merge request.
// A sort by committed date would be wrong for commits with the same date,
// which is common after a rebase, and for commits with a wrong clock.
func (p *project) listMergeRequestCommits(ctx context.Context, mrIID int64) ([]*gogitlab.Commit, error) {
	var commits []*gogitlab.Commit
	opts := &gogitlab.GetMergeRequestCommitsOptions{ListOptions: gogitlab.ListOptions{PerPage: 100}}
	for {
		page, resp, err := p.m.gl.MergeRequests.GetMergeRequestCommits(p.project.ID, mrIID, opts, gogitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		commits = append(commits, page...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	slices.Reverse(commits)
	return commits, nil
}

// listMergeRequestAwardEmoji returns all award emoji of the merge request.
func (p *project) listMergeRequestAwardEmoji(ctx context.Context, mrIID int64) ([]*gogitlab.AwardEmoji, error) {
	var awards []*gogitlab.AwardEmoji
	opts := &gogitlab.ListAwardEmojiOptions{ListOptions: gogitlab.ListOptions{PerPage: 100}}
	for {
		page, resp, err := p.m.gl.AwardEmoji.ListMergeRequestAwardEmoji(p.project.ID, mrIID, opts, gogitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		awards = append(awards, page...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}
	return awards, nil
}

// listMergeRequestNotes returns all notes of the merge request, the oldest
// first.
func (p *project) listMergeRequestNotes(ctx context.Context, mrIID int64) ([]*gogitlab.Note, error) {
	var notes []*gogitlab.Note
	opts := &gogitlab.ListMergeRequestNotesOptions{
		ListOptions: gogitlab.ListOptions{PerPage: 100},
		OrderBy:     Pointer("created_at"),
		Sort:        Pointer("asc"),
	}
	for {
		page, resp, err := p.m.gl.Notes.ListMergeRequestNotes(p.project.ID, mrIID, opts, gogitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		notes = append(notes, page...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}
	return notes, nil
}

// createLocalBranch creates a branch ref at hash in the local mirror. Only the ref
// is needed for the push, so it skips the worktree checkout that
// Worktree.Checkout would do. Like Checkout with Create, it fails when the
// branch already exists.
func (p *project) createLocalBranch(name plumbing.ReferenceName, hash plumbing.Hash) error {
	if err := name.Validate(); err != nil {
		return err
	}
	if _, err := p.repo.Storer.Reference(name); err == nil {
		return fmt.Errorf("a branch named %q already exists", name)
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	return p.repo.Storer.SetReference(plumbing.NewHashReference(name, hash))
}

// fetchedCommitRefPrefix is the namespace of the local references that keep
// the commits that fetchMissingCommits fetched. pushToGitHub pushes only
// branches and tags, so these references never reach GitHub.
const fetchedCommitRefPrefix = "refs/gitlab-migrator/fetched/"

// fetchMissingCommits fetches the commits of hashes that the local clone does
// not have from the remote "gitlab", by their SHA. The mirror clone has all
// references that GitLab advertises, also refs/merge-requests/<iid>/head. But
// the commits of a closed or merged merge request whose source branch was
// deleted can be reachable only from references that GitLab hides, for
// example refs/keep-around/..., and then the clone lacks them (issue #137). The
// fetch works only when the server serves commits that it does not advertise
// (uploadpack.allowAnySHA1InWant, or allowTipSHA1InWant for hidden refs);
// otherwise it returns an error, and go-git returns
// git.ErrExactSHA1NotSupported when the server does not offer it at all.
//
// The commits are fetched one by one, in the order of hashes, and a commit
// that an earlier fetch brought as an ancestor is not fetched again. So the
// caller passes the end commit first: its fetch brings the start commit too.
// A server with only allowTipSHA1InWant serves the end commit, the tip of the
// hidden ref, but would refuse a request that also names the start commit. A
// fetched commit gets a reference under fetchedCommitRefPrefix.
func (p *project) fetchMissingCommits(ctx context.Context, mergeRequestIID int64, hashes ...plumbing.Hash) error {
	for _, hash := range hashes {
		if _, err := p.repo.Storer.EncodedObject(plumbing.CommitObject, hash); err == nil {
			continue
		} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return fmt.Errorf("looking up commit %s: %w", hash, err)
		}

		p.log.Info("fetching a commit of the merge request that the clone does not have, its source branch was probably deleted", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequestIID, "sha", hash)
		err := p.repo.FetchContext(ctx, &git.FetchOptions{
			RemoteName: "gitlab",
			RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(fmt.Sprintf("+%[1]s:%[2]s%[1]s", hash, fetchedCommitRefPrefix))},
			Tags:       git.NoTags,
		})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			// The URL of the remote holds the GitLab token, and go-git puts
			// the URL into the text of an HTTP error. The error names the
			// hash: the caller reports the missing commit, which can be
			// another one when this fetch stops the loop.
			return fmt.Errorf("fetching commit %s: %w", hash, &redactedError{err: err, secret: p.m.cfg.GitlabToken})
		}
	}
	return nil
}

// redactedError hides secret in the text of err. errors.Is and errors.As
// still see err.
type redactedError struct {
	err    error
	secret string
}

func (e *redactedError) Error() string {
	text := e.err.Error()
	if e.secret == "" {
		return text
	}
	// A URL can hold the secret escaped.
	for _, form := range []string{e.secret, url.PathEscape(e.secret), strings.TrimPrefix(url.UserPassword("", e.secret).String(), ":")} {
		text = strings.ReplaceAll(text, form, "REDACTED")
	}
	return text
}

func (e *redactedError) Unwrap() error {
	return e.err
}

// isTransientFetchError reports whether err of fetchMissingCommits can be gone
// in a later run: a network error, a timeout, an HTTP status 401, 403, 408,
// 429 or 5xx, or a response that ends too early, for example because a proxy
// closed the connection during the download of the pack. A server that does
// not serve the commit gives another error. 401 and 403 can go away with a new
// token or when GitLab lifts a block of the IP.
func isTransientFetchError(err error) bool {
	// go-git returns 401 and 403 as these transport errors, not as a
	// githttp.Err.
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return true
	}
	// go-git wraps other HTTP errors in a plumbing.UnexpectedError, which has
	// no Unwrap method.
	var unexpected *plumbing.UnexpectedError
	if errors.As(err, &unexpected) {
		err = unexpected.Err
	}
	// go-git wraps the error of a cut pack in packfile.ErrMalformedPackFile
	// with %w.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var httpErr *githttp.Err
	if errors.As(err, &httpErr) {
		status := httpErr.StatusCode()
		return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded)
}

// missingCommitHint explains a failed lookup of a commit of a closed or merged
// merge request. lookupErr is the error of the lookup, fetchErr the error of
// fetchMissingCommits. It returns "" when the commit exists but cannot be read.
func missingCommitHint(lookupErr, fetchErr error) string {
	if !errors.Is(lookupErr, plumbing.ErrObjectNotFound) {
		return ""
	}
	fetchResult := "fetching it from GitLab by its SHA did not get it"
	if fetchErr != nil {
		fetchResult = fmt.Sprintf("fetching the missing commits from GitLab by their SHA failed: %v", fetchErr)
	}
	return fmt.Sprintf(" (no ref that GitLab shows has the commit, for example because the source branch was deleted, and %s; use -skip-invalid-merge-requests to skip such merge requests)", fetchResult)
}

// missingCommitSkipReason is the skip reason for a commit of a closed or
// merged merge request that the lookup did not find. name is "start commit"
// or "end commit"; lookupErr and fetchErr are as for missingCommitHint. The
// reasons differ from the old ones that ShouldSkip migrates again.
func missingCommitSkipReason(name string, lookupErr, fetchErr error) string {
	if !errors.Is(lookupErr, plumbing.ErrObjectNotFound) {
		return name + " cannot be read"
	}
	if fetchErr != nil {
		return fmt.Sprintf("%s is not in the clone and fetching the missing commits from GitLab failed: %v", name, fetchErr)
	}
	return name + " is not in the clone and fetching it from GitLab did not get it"
}

// editPullRequest edits a pull request and retries on 404. The result goes to a
// local variable, so req stays unchanged between attempts and may be the
// caller's own pull request.
func (p *project) editPullRequest(ctx context.Context, desc string, number int, req *gogithub.PullRequest) (*gogithub.PullRequest, error) {
	var pr *gogithub.PullRequest
	err := p.retryOnNotFound(ctx, desc, func() error {
		var editErr error
		pr, _, editErr = p.m.gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], number, req)
		return editErr
	})
	return pr, err
}

// retargetPullRequestsBeforeTrim gives the open pull requests on an old trunk
// the GitHub trunk as base branch, when the trim is about to delete that old
// trunk on GitHub. GitHub closes the open pull requests whose base branch is
// deleted, and a closed pull request cannot get a new base, so the change in
// migrateMergeRequest would come too late. An old trunk is the GitLab trunk
// when it is renamed, or githubDefaultBranch, the default branch of the GitHub
// repository before the run, when this run has another trunk (for example an
// earlier run had a rename that this run does not have). branchesToDelete are
// the branches the trim deletes.
func (p *project) retargetPullRequestsBeforeTrim(ctx context.Context, branchesToDelete []string, githubDefaultBranch string) error {
	oldTrunks := []string{p.project.DefaultBranch}
	if githubDefaultBranch != p.project.DefaultBranch {
		oldTrunks = append(oldTrunks, githubDefaultBranch)
	}
	for _, oldTrunk := range oldTrunks {
		if oldTrunk == "" || oldTrunk == p.defaultBranch || !slices.Contains(branchesToDelete, oldTrunk) {
			continue
		}
		if err := p.retargetOpenPullRequests(ctx, oldTrunk); err != nil {
			return err
		}
	}
	return nil
}

// retargetOpenPullRequests gives the open pull requests on oldTrunk the GitHub
// trunk as base branch.
func (p *project) retargetOpenPullRequests(ctx context.Context, oldTrunk string) error {
	// All pages first: each edit takes a pull request out of the filtered list,
	// which would shift the later pages.
	opts := &gogithub.PullRequestListOptions{
		State:       "open",
		Base:        oldTrunk,
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	var pullRequests []*gogithub.PullRequest
	for {
		prs, resp, err := p.m.gh.PullRequests.List(ctx, p.githubPath[0], p.githubPath[1], opts)
		if err != nil {
			return fmt.Errorf("listing open pull requests on old trunk %s: %w", oldTrunk, err)
		}
		pullRequests = append(pullRequests, prs...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	for _, pr := range pullRequests {
		p.log.Info("changing base branch of pull request before trimming the old trunk", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pr.GetNumber(), "old_base", oldTrunk, "new_base", p.defaultBranch)
		edit := &gogithub.PullRequest{Base: &gogithub.PullRequestBranch{Ref: Pointer(p.defaultBranch)}}
		if _, err := p.editPullRequest(ctx, "changing base branch of pull request", pr.GetNumber(), edit); err != nil {
			return fmt.Errorf("changing base branch of pull request %d to %s: %w", pr.GetNumber(), p.defaultBranch, err)
		}
	}
	return nil
}

// retargetSavedPullRequest gives pull request prNumber the GitHub trunk as
// base branch. It is for a merge request that -state-dir records as migrated,
// so migrateMergeRequest does not run for it. It does something only when the
// GitLab trunk is renamed and the merge request is open and targets the GitLab
// trunk; the pull request of a closed or merged merge request targets its
// temporary branch. With -skip-open-merge-requests it does nothing, like the
// run without -state-dir, which skips open merge requests. GitHub refuses a new
// base for a closed pull request, so a closed one keeps its base. Only the base
// changes: title, body and state stay as the saved result left them.
func (p *project) retargetSavedPullRequest(ctx context.Context, mergeRequest *gogitlab.BasicMergeRequest, prNumber *int) error {
	if prNumber == nil || p.m.cfg.SkipOpenMergeRequests || p.defaultBranch == p.project.DefaultBranch ||
		!strings.EqualFold(mergeRequest.State, "opened") || mergeRequest.TargetBranch != p.project.DefaultBranch {
		return nil
	}

	pr, err := p.m.ghClient.GetPullRequest(ctx, p.githubPath[0], p.githubPath[1], *prNumber)
	if err != nil {
		return fmt.Errorf("retrieving pull request %d: %w", *prNumber, err)
	}
	if pr.GetState() != "open" || pr.GetBase().GetRef() == p.defaultBranch {
		return nil
	}

	p.log.Info("changing base branch of migrated pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", *prNumber, "old_base", pr.GetBase().GetRef(), "new_base", p.defaultBranch)
	edit := &gogithub.PullRequest{Base: &gogithub.PullRequestBranch{Ref: Pointer(p.defaultBranch)}}
	if _, err := p.editPullRequest(ctx, "changing base branch of pull request", *prNumber, edit); err != nil {
		return fmt.Errorf("changing base branch of pull request %d to %s: %w", *prNumber, p.defaultBranch, err)
	}
	return nil
}

// createTempBranchesViaAPI creates the temporary target and source branches of
// the closed merge request mr on GitHub. It reports true when it skips the
// merge request. When it skips the merge request or fails, it deletes the
// branches that it created itself, because the caller only cleans up after a
// success. A branch that already existed, for example from an earlier run, is
// not deleted then: this call did not create it.
func (p *project) createTempBranchesViaAPI(ctx context.Context, mr *gogitlab.BasicMergeRequest, commits []*gogitlab.Commit, result *MergeRequestResult) (skipped bool, err error) {
	owner := p.githubPath[0]
	repo := p.githubPath[1]
	startShortID := commits[0].ShortID
	endShortID := commits[len(commits)-1].ShortID

	var created []string
	defer func() {
		if !skipped && err == nil {
			return
		}
		for _, branch := range created {
			p.log.Debug("deleting temporary branch via API as the merge request was not migrated", "owner", owner, "repo", repo, "merge_request_id", mr.IID, "branch", branch)
			p.deleteTempBranchViaAPI(ctx, branch)
		}
	}()

	// No retry on GetCommit: a 404 here means the commit genuinely does not exist on GitHub
	// (e.g. force-pushed away), not an eventual-consistency delay. The mirror push completes
	// before we reach this point, so git objects are already available.
	p.log.Trace("inspecting start commit via GitHub API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "sha", startShortID)
	startCommit, _, err := p.m.gh.Git.GetCommit(ctx, owner, repo, commits[0].ID)
	if err != nil {
		if isGitHubNotFound(err) {
			if p.m.cfg.SkipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as start commit does not exist on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "missing_commit", startShortID)
				result.Status = StatusSkipped
				result.SkipReason = "start commit does not exist on GitHub"
				return true, nil
			}
		}
		return false, fmt.Errorf("loading start commit %s from GitHub: %w", startShortID, err)
	}

	if len(startCommit.Parents) == 0 {
		if p.m.cfg.SkipInvalidMergeRequests {
			p.log.Info("skipping invalid merge request as start commit has no parents", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "sha", startShortID)
			result.Status = StatusSkipped
			result.SkipReason = "start commit has no parents (orphaned)"
			return true, nil
		}
		return false, fmt.Errorf("start commit %s for merge request %d has no parents", startShortID, mr.IID)
	}

	parentSHA := startCommit.Parents[0].GetSHA()
	p.log.Trace("creating target branch for merged/closed merge request via API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "branch", mr.TargetBranch, "sha", parentSHA)
	// Retry: GitHub's references layer may lag behind the object store — CreateRef can
	// 404 on a SHA that GetCommit already resolved, due to cross-service propagation delay.
	err = p.retryOnNotFound(ctx, "creating target branch", func() error {
		_, _, createErr := p.m.gh.Git.CreateRef(ctx, owner, repo, gogithub.CreateRef{Ref: "refs/heads/" + mr.TargetBranch, SHA: parentSHA})
		return createErr
	})
	if err == nil {
		created = append(created, mr.TargetBranch)
	} else {
		if isAlreadyExistsError(err) {
			p.log.Trace("temporary target branch already exists on GitHub", "branch", mr.TargetBranch)
		} else if isReferenceUpdateFailedError(err) && p.m.cfg.SkipInvalidMergeRequests {
			p.log.Info("skipping invalid merge request as target branch could not be created on GitHub (parent commit unreachable)", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "branch", mr.TargetBranch, "sha", parentSHA)
			result.Status = StatusSkipped
			result.SkipReason = "target branch creation failed (parent commit unreachable on GitHub)"
			return true, nil
		} else {
			return false, fmt.Errorf("creating temporary target branch %s on GitHub: %w", mr.TargetBranch, err)
		}
	}

	// No retry on GetCommit — same reasoning as above.
	p.log.Trace("inspecting end commit via GitHub API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "sha", endShortID)
	if _, _, err = p.m.gh.Git.GetCommit(ctx, owner, repo, commits[len(commits)-1].ID); err != nil {
		if isGitHubNotFound(err) {
			if p.m.cfg.SkipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as end commit does not exist on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "missing_commit", endShortID)
				result.Status = StatusSkipped
				result.SkipReason = "end commit does not exist on GitHub"
				return true, nil
			}
		}
		return false, fmt.Errorf("loading end commit %s from GitHub: %w", endShortID, err)
	}

	endSHA := commits[len(commits)-1].ID
	p.log.Trace("creating source branch for merged/closed merge request via API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "branch", mr.SourceBranch, "sha", endSHA)
	// Retry for same reason as target branch above.
	err = p.retryOnNotFound(ctx, "creating source branch", func() error {
		_, _, createErr := p.m.gh.Git.CreateRef(ctx, owner, repo, gogithub.CreateRef{Ref: "refs/heads/" + mr.SourceBranch, SHA: endSHA})
		return createErr
	})
	if err == nil {
		created = append(created, mr.SourceBranch)
	} else {
		if isAlreadyExistsError(err) {
			p.log.Trace("temporary source branch already exists on GitHub", "branch", mr.SourceBranch)
		} else {
			// 422 Reference update failed is not expected here: endSHA is the last commit of the MR,
			// which was just verified to exist on GitHub via GetCommit above, so it is always reachable.
			return false, fmt.Errorf("creating temporary source branch %s on GitHub: %w", mr.SourceBranch, err)
		}
	}

	return false, nil
}

// deleteTempBranches deletes the temporary branches of a closed merge request
// on GitHub after its pull request prNumber exists: with -pull-requests-only by
// API, else by a push to the remote "github" of the local clone. With
// onlyListed it deletes only the branches that the cached branch list of the
// GitHub repository has. A branch that does not exist is not an error, for
// example when a run before deleted it already. A failure is only logged: the
// branches stay and do no harm. When the run was stopped, nothing more is
// tried, and the stop is not logged as an error: no request can work with the
// cancelled ctx. Then deleteTempBranches returns true. The next run that
// migrates the merge request again finds the pull request and deletes the
// branches then: a stopped run that still finishes the merge request records
// it as partial (as failed when it was skipped), so -state-dir does not skip it.
func (p *project) deleteTempBranches(ctx context.Context, prNumber int, onlyListed bool, branches ...string) (stopped bool) {
	keep := func() bool {
		p.log.Debug("keeping temporary branches for closed pull request because the run was stopped", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", prNumber, "branches", branches)
		return true
	}
	if ctx.Err() != nil {
		return keep()
	}

	if onlyListed {
		branches = p.listedBranches(ctx, branches)
		if ctx.Err() != nil {
			return keep()
		}
		if len(branches) == 0 {
			p.log.Trace("temporary branches for closed pull request are not on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", prNumber)
			return false
		}
	}

	if p.m.cfg.PullRequestsOnly {
		p.log.Debug("deleting temporary branches for closed pull request via API", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", prNumber, "branches", branches)
		for _, branch := range branches {
			if ctx.Err() != nil || p.deleteTempBranchViaAPI(ctx, branch) {
				return keep()
			}
		}
		return false
	}

	p.log.Debug("deleting temporary branches for closed pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", prNumber, "branches", branches)
	// A push deletes only the branches that the remote has. When it has none
	// of them, the push reports NoErrAlreadyUpToDate.
	refSpecs := make([]gitconfig.RefSpec, 0, len(branches))
	for _, branch := range branches {
		refSpecs = append(refSpecs, gitconfig.RefSpec(fmt.Sprintf(":refs/heads/%s", branch)))
	}
	cleanupOpts := &git.PushOptions{
		RemoteName: "github",
		RefSpecs:   refSpecs,
		Force:      true,
	}
	sideband, err := p.pushWithSideband(ctx, cleanupOpts)
	switch {
	case err == nil:
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		p.log.Trace("branches already deleted on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", prNumber, "branches", branches)
	case ctx.Err() != nil:
		return keep()
	default:
		p.log.Error(formatPushError("pushing branch deletions to github", "", err, sideband).Error())
	}
	return false
}

// listedBranches returns the names that the cached branch list of the GitHub
// repository has. The list can be older than a deletion, for example of the
// trim of -trim-branches-on-github: such a branch is returned, and its deletion
// finds nothing. When the list cannot be read, listedBranches logs a warning and
// returns all names, so their deletion is still tried. When the run was stopped,
// it logs no warning: the caller checks ctx.
func (p *project) listedBranches(ctx context.Context, names []string) []string {
	githubBranches, err := p.m.ghClient.GetBranches(ctx, p.githubPath[0], p.githubPath[1])
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("listing the branches on GitHub failed, trying to delete the temporary branches anyway", "owner", p.githubPath[0], "repo", p.githubPath[1], "branches", names, "error", err)
		}
		return names
	}
	listed := make([]string, 0, len(names))
	for _, name := range names {
		if slices.ContainsFunc(githubBranches, func(b *gogithub.Branch) bool { return b.GetName() == name }) {
			listed = append(listed, name)
		}
	}
	return listed
}

// deleteTempBranchViaAPI deletes the temporary branch on GitHub. A branch that
// does not exist is not an error. Another failure is only logged: the branch
// stays and does no harm. When the run was stopped, the failure is not logged
// and deleteTempBranchViaAPI returns true.
func (p *project) deleteTempBranchViaAPI(ctx context.Context, branch string) (stopped bool) {
	_, err := p.m.gh.Git.DeleteRef(ctx, p.githubPath[0], p.githubPath[1], "refs/heads/"+branch)
	switch {
	case err == nil:
	case isGitHubNotFound(err) || isReferenceDoesNotExistError(err):
		p.log.Trace("temporary branch already deleted on GitHub", "branch", branch)
	case ctx.Err() != nil:
		return true
	default:
		p.log.Warn("failed to delete temporary branch via API", "branch", branch, "error", err)
	}
	return false
}

// githubMention returns fallback when the GitLab user has no website set. Otherwise it
// returns "@" plus the lowercased website URL without a leading "https://github.com/".
// The URL is not checked to be a GitHub profile.
func githubMention(u *gogitlab.User, fallback string) string {
	if u.WebsiteURL == "" {
		return fallback
	}
	return "@" + strings.TrimPrefix(strings.ToLower(u.WebsiteURL), "https://github.com/")
}

// commentTextHeading is the line of a migrated comment that ends the generated
// header and starts the original text of the GitLab note.
const commentTextHeading = "## Original Comment"

// bodyMatchesNote reports whether body is the text of a comment that the tool
// migrated for the GitLab note noteID. It matches the whole table cell
// including the closing pipe, so note 12 does not match the comment of note 123.
// It looks only at the header the tool generates, which ends before the
// original text: users write that text, and it can quote another header.
// The heading must be a whole line, so a line like "## Original Commentary" does
// not end the header. The line may end with CRLF: the GitHub web editor saves
// comments that way. A body without the heading is not a comment of the tool,
// so it never matches.
func bodyMatchesNote(body string, noteID int64) bool {
	loc := commentTextHeadingLine.FindStringIndex(body)
	if loc == nil {
		return false
	}
	return strings.Contains(body[:loc[0]], fmt.Sprintf("**Note ID** | %d |", noteID))
}

// commentTextHeadingLine finds commentTextHeading as a whole line, with the line
// break in front of it.
var commentTextHeadingLine = regexp.MustCompile(`\n` + regexp.QuoteMeta(commentTextHeading) + `\r?(\n|$)`)

// findMigratedComment returns the first of prComments that was migrated for the
// GitLab note noteID, or nil when there is none.
func findMigratedComment(prComments []*gogithub.IssueComment, noteID int64) *gogithub.IssueComment {
	for _, prComment := range prComments {
		if prComment != nil && bodyMatchesNote(prComment.GetBody(), noteID) {
			return prComment
		}
	}
	return nil
}

func bodyMatchesMergeRequest(body string, mrIID int64) bool {
	return strings.Contains(body, fmt.Sprintf("**GitLab MR Number** | %d |", mrIID)) ||
		strings.Contains(body, fmt.Sprintf("**GitLab MR Number** | [%d]", mrIID))
}

// findExistingPRByList looks up an already-created PR using PullRequests.List
// instead of the Search API. This is used as a fallback when the Search API
// index lags behind and a subsequent Create returns 422 "already exists".
func (p *project) findExistingPRByList(ctx context.Context, mr *gogitlab.BasicMergeRequest) (*gogithub.PullRequest, error) {
	for _, head := range []string{
		fmt.Sprintf("%s:%s", p.githubPath[0], mr.SourceBranch),
		fmt.Sprintf("%s:migration-source-%d/%s", p.githubPath[0], mr.IID, mr.SourceBranch),
	} {
		opts := &gogithub.PullRequestListOptions{
			Head:        head,
			State:       "all",
			ListOptions: gogithub.ListOptions{PerPage: 100},
		}
		for {
			prs, resp, err := p.m.gh.PullRequests.List(ctx, p.githubPath[0], p.githubPath[1], opts)
			if err != nil {
				return nil, err
			}
			for _, pr := range prs {
				if bodyMatchesMergeRequest(pr.GetBody(), mr.IID) {
					return pr, nil
				}
			}
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
	return nil, nil
}

// as422 returns the GitHub error response when err is a 422 Unprocessable Entity.
func as422(err error) (*gogithub.ErrorResponse, bool) {
	var ghErr *gogithub.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr == nil || ghErr.Response == nil ||
		ghErr.Response.StatusCode != http.StatusUnprocessableEntity {
		return nil, false
	}
	return ghErr, true
}

// is422Matching reports whether err is a GitHub 422 response whose top-level
// message or any of its detail messages satisfies match.
func is422Matching(err error, match func(string) bool) bool {
	ghErr, ok := as422(err)
	if !ok {
		return false
	}
	if match(ghErr.Message) {
		return true
	}
	for _, e := range ghErr.Errors {
		if match(e.Message) {
			return true
		}
	}
	return false
}

func isAlreadyExistsError(err error) bool {
	return is422Matching(err, func(m string) bool { return strings.Contains(m, "Reference already exists") })
}

// isReferenceDoesNotExistError reports whether GitHub refused to delete a
// branch because it does not exist.
func isReferenceDoesNotExistError(err error) bool {
	return is422Matching(err, func(m string) bool { return strings.Contains(m, "Reference does not exist") })
}

func isReferenceUpdateFailedError(err error) bool {
	ghErr, ok := as422(err)
	// GitHub returns "Reference update failed" with an empty errors array for this condition,
	// so checking only the top-level message is sufficient and intentional.
	return ok && strings.Contains(ghErr.Message, "Reference update failed")
}

func isAlreadyExistsPRError(err error) bool {
	return is422Matching(err, func(m string) bool { return strings.Contains(m, "A pull request already exists") })
}

func isSearchSyntaxError(err error) bool {
	return is422Matching(err, containsSearchSyntaxHint)
}

func containsSearchSyntaxHint(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "search is invalid") ||
		(strings.Contains(lower, "search query") && strings.Contains(lower, "invalid syntax"))
}

// archivedRepoHint is added to the error of a write to an archived GitHub
// repository when -unarchive-archived-repos is not set.
const archivedRepoHint = "the repository is archived; use -unarchive-archived-repos to unarchive it for the migration"

// addArchivedHint adds archivedRepoHint to err when err comes from a write to
// an archived repository and -unarchive-archived-repos is not set. Any other
// err is returned as it is.
func (p *project) addArchivedHint(err error) error {
	if isArchivedRepoError(err) && !p.m.cfg.UnarchiveArchivedRepos {
		return fmt.Errorf("%w (%s)", err, archivedRepoHint)
	}
	return err
}

// isArchivedRepoError reports whether err is the 403 that GitHub answers to a
// write to an archived repository ("Repository was archived so is read-only").
func isArchivedRepoError(err error) bool {
	var ghErr *gogithub.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr == nil || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusForbidden {
		return false
	}
	return strings.Contains(strings.ToLower(ghErr.Message), "archived")
}

func isGitHubNotFound(err error) bool {
	var ghErr *gogithub.ErrorResponse
	if errors.As(err, &ghErr) {
		return ghErr != nil && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
	}
	return false
}

func (p *project) retryOnNotFound(ctx context.Context, desc string, fn func() error) error {
	retryDelays := [...]time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}

	err := fn()
	if err == nil || !isGitHubNotFound(err) {
		return err
	}

	for i, delay := range retryDelays {
		p.log.Warn("GitHub API returned 404, retrying", "operation", desc, "attempt", i+1, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		err = fn()
		if err == nil || !isGitHubNotFound(err) {
			return err
		}
	}
	p.log.Warn("retries exhausted, still 404", "operation", desc, "attempts", len(retryDelays)+1)
	return err
}

// rearchiveGracePeriod is how long the re-archive of a temporarily unarchived
// repository may still run after the migration was canceled (Ctrl+C). It
// covers the retries of setArchivedWithRetry. A variable, so tests can make it
// short.
var rearchiveGracePeriod = 2 * time.Minute

// detachedContext returns a context with the values of ctx that is not
// canceled together with ctx. Once ctx is done, the returned context ends
// after grace. Call stop when the work is done.
func detachedContext(ctx context.Context, grace time.Duration) (detached context.Context, stop func()) {
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopAfter := context.AfterFunc(ctx, func() {
		timer := time.AfterFunc(grace, cancel)
		context.AfterFunc(detached, func() { timer.Stop() })
	})
	return detached, func() {
		stopAfter()
		cancel()
	}
}

func (p *project) setArchived(ctx context.Context, archived bool) error {
	update := gogithub.Repository{Archived: Pointer(archived)}
	_, _, err := p.m.gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &update)
	return err
}

func (p *project) setArchivedWithRetry(ctx context.Context, archived bool) error {
	retryDelays := [...]time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}
	action := "archive"
	if !archived {
		action = "unarchive"
	}

	err := p.setArchived(ctx, archived)
	if err == nil {
		return nil
	}

	for i, delay := range retryDelays {
		p.log.Warn("failed to "+action+" GitHub repo, retrying", "attempt", i+1, "max_attempts", len(retryDelays)+1, "delay", delay, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		err = p.setArchived(ctx, archived)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("after %d attempts: %w", len(retryDelays)+1, err)
}

