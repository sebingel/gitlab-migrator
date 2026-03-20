package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	plumbingcache "github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	gogitlab "github.com/xanzy/go-gitlab"
	"github.com/manicminer/gitlab-migrator/internal/config"
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
	storageType   string
	storageDir    string
	storagePath   string
	pushBatchSize int
	result        ProjectResult
}

func (m *Migrator) newProject(slugs []string) (*project, error) {
	var err error
	p := &project{
		m:             m,
		storageType:   m.Cfg.StorageType,
		storageDir:    m.Cfg.StorageDir,
		pushBatchSize: m.Cfg.PushBatchSize,
	}
	p.log = m.Logger.Named(slugs[0])

	p.gitlabPath, p.githubPath, err = ParseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %w", err)
	}

	p.log.Info("searching for GitLab project", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	p.project, _, err = m.GL.Projects.GetProject(slugs[0], nil)
	if err != nil {
		return nil, fmt.Errorf("retrieving project: %w", err)
	}

	if p.project == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	p.defaultBranch = "main"
	if m.Cfg.RenameTrunkBranch != "" {
		p.defaultBranch = m.Cfg.RenameTrunkBranch
	} else if !m.Cfg.RenameMasterToMain && p.project.DefaultBranch != "" {
		p.defaultBranch = p.project.DefaultBranch
	}

	return p, nil
}

func (p *project) createGitStorage() (storage.Storer, error) {
	if p.storageType == "filesystem" {
		var baseDir string
		if p.storageDir != "" {
			baseDir = p.storageDir
		} else {
			baseDir = os.TempDir()
		}

		tempDir, err := os.MkdirTemp(baseDir, fmt.Sprintf("gitlab-migrator-%s-%s-*", p.gitlabPath[0], p.gitlabPath[1]))
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
		Private:       Pointer(true),
		HasIssues:     Pointer(true),
		HasProjects:   Pointer(true),
		HasWiki:       Pointer(true),
	}
	if _, _, err := p.m.GH.Repositories.Create(ctx, p.githubPath[0], &newRepo); err != nil {
		return fmt.Errorf("creating github repo: %w", err)
	}
	return nil
}

func pushErrHint(err error) string {
	if err != nil && strings.Contains(err.Error(), "without 'workflow' scope") {
		return " (hint: add 'workflow' scope to your GitHub token to push workflow files)"
	}
	return ""
}

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
var gitProgressLineRegex = regexp.MustCompile(`(?i)^(Compressing|Counting|Enumerating|Receiving|Resolving|Writing) (objects|deltas)\b`)

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

func (p *project) migrate(ctx context.Context) (ProjectResult, error) {
	p.result = ProjectResult{
		GitLabGroup:      p.gitlabPath[0],
		GitLabProject:    p.gitlabPath[1],
		GitHubOwner:      p.githubPath[0],
		GitHubRepo:       p.githubPath[1],
		StartTime:        time.Now(),
		BranchesMigrated: make([]string, 0),
		MergeRequests:    make([]MergeRequestResult, 0),
	}

	p.log.Debug("checking for existing repository on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1])
	_, _, err := p.m.GH.Repositories.Get(ctx, p.githubPath[0], p.githubPath[1])

	var githubError *gogithub.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return p.result, fmt.Errorf("retrieving github repo: %w", err)
	}

	if p.m.Cfg.PullRequestsOnly {
		if err != nil {
			return p.result, fmt.Errorf("GitHub repository %s/%s not found (-pull-requests-only requires the repository to already exist on GitHub)", p.githubPath[0], p.githubPath[1])
		}
		p.log.Info("pull-requests-only mode: skipping repository clone and push", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	} else {
		cloneUrl, parseErr := url.Parse(p.project.HTTPURLToRepo)
		if parseErr != nil {
			return p.result, fmt.Errorf("parsing clone URL: %v", parseErr)
		}

		p.log.Info("mirroring repository from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_org", p.githubPath[0], "github_repo", p.githubPath[1], "force", !p.m.Cfg.NoForce)

		homepage := fmt.Sprintf("https://%s/%s/%s", p.m.Cfg.GitlabDomain, p.gitlabPath[0], p.gitlabPath[1])

		if err != nil {
			if err = p.createRepo(ctx, homepage, false); err != nil {
				return p.result, err
			}
		} else if p.m.Cfg.DeleteExistingRepos {
			p.log.Warn("existing repository was found on GitHub, proceeding to delete", "owner", p.githubPath[0], "repo", p.githubPath[1])
			if _, err = p.m.GH.Repositories.Delete(ctx, p.githubPath[0], p.githubPath[1]); err != nil {
				return p.result, fmt.Errorf("deleting existing github repo: %w", err)
			}

			if err = p.createRepo(ctx, homepage, true); err != nil {
				return p.result, err
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
		if _, _, err = p.m.GH.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
			return p.result, fmt.Errorf("updating github repo: %w", err)
		}

		cloneUrl.User = url.UserPassword("oauth2", p.m.Cfg.GitlabToken)
		cloneUrlWithCredentials := cloneUrl.String()

		stor, err := p.createGitStorage()
		if err != nil {
			return p.result, fmt.Errorf("creating git storage: %w", err)
		}

		defer p.cleanupStorage()

		fs := memfs.New()

		p.log.Debug("cloning repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", p.project.HTTPURLToRepo)
		p.repo, err = git.CloneContext(ctx, stor, fs, &git.CloneOptions{
			URL:        cloneUrlWithCredentials,
			Auth:       nil,
			RemoteName: "gitlab",
			Mirror:     true,
		})
		if err != nil {
			return p.result, fmt.Errorf("cloning gitlab repo: %w", err)
		}

		if p.defaultBranch != p.project.DefaultBranch {
			if gitlabTrunk, err := p.repo.Reference(plumbing.NewBranchReferenceName(p.project.DefaultBranch), false); err == nil {
				p.log.Info("renaming trunk branch prior to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())

				p.log.Debug("creating new trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())
				githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(p.defaultBranch), gitlabTrunk.Hash())
				if err = p.repo.Storer.SetReference(githubTrunk); err != nil {
					return p.result, fmt.Errorf("creating trunk branch: %w", err)
				}

				p.log.Debug("deleting old trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "sha", gitlabTrunk.Hash())
				if err = p.repo.Storer.RemoveReference(gitlabTrunk.Name()); err != nil {
					return p.result, fmt.Errorf("deleting old trunk branch: %w", err)
				}
			}
		}

		githubUrl := fmt.Sprintf("https://%s/%s/%s", p.m.Cfg.GithubDomain, p.githubPath[0], p.githubPath[1])
		githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", p.m.Cfg.GithubUser, p.m.Cfg.GithubToken, p.m.Cfg.GithubDomain, p.githubPath[0], p.githubPath[1])

		p.log.Debug("adding remote for GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		if _, err = p.repo.CreateRemote(&gitconfig.RemoteConfig{
			Name:   "github",
			URLs:   []string{githubUrlWithCredentials},
			Mirror: true,
		}); err != nil {
			return p.result, fmt.Errorf("adding github remote: %w", err)
		}

		p.log.Debug("determining branches to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		branches, err := p.repo.Branches()
		if err != nil {
			return p.result, fmt.Errorf("retrieving branches: %w", err)
		}

		gitlabBranches := make([]string, 0)
		refSpecs := make([]gitconfig.RefSpec, 0)
		if err = branches.ForEach(func(ref *plumbing.Reference) error {
			branchName := ref.Name().Short()
			gitlabBranches = append(gitlabBranches, branchName)
			p.result.BranchesMigrated = append(p.result.BranchesMigrated, branchName)
			refSpecs = append(refSpecs, gitconfig.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref.Name())))
			return nil
		}); err != nil {
			return p.result, fmt.Errorf("parsing branches: %w", err)
		}

		batches := ChunkRefSpecs(refSpecs, p.pushBatchSize)
		pushMode := "force-pushing"
		if p.m.Cfg.NoForce {
			pushMode = "pushing"
		}
		p.log.Debug(pushMode+" branches to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "total_branches", len(refSpecs), "batches", len(batches), "batch_size", p.pushBatchSize)

		for batchNum, batch := range batches {
			p.log.Debug("pushing branch batch", "name", p.gitlabPath[1], "batch", batchNum+1, "total_batches", len(batches), "branches_in_batch", len(batch))

			opts := &git.PushOptions{
				RemoteName: "github",
				Force:      !p.m.Cfg.NoForce,
				RefSpecs:   batch,
			}
			sideband, err := p.pushWithSideband(ctx, opts)
			if err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					p.log.Debug("batch already up-to-date", "batch", batchNum+1)
				} else {
					msg := fmt.Sprintf("pushing branch batch %d/%d to github", batchNum+1, len(batches))
					hint := pushErrHint(err)
					if p.m.Cfg.NoForce {
						hint = " (hint: remove -no-force if push is rejected due to conflicts)" + hint
					}
					return p.result, formatPushError(msg, hint, err, sideband)
				}
			}
		}

		if p.m.Cfg.TrimGithubBranches {
			p.log.Debug("determining old branches to trim on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
			refSpecsToDelete := make([]gitconfig.RefSpec, 0)
			githubBranches, err := p.m.GHClient.GetBranches(ctx, p.githubPath[0], p.githubPath[1])
			if err != nil {
				return p.result, fmt.Errorf("listing branches from GitHub: %w", err)
			}
			for _, githubBranch := range githubBranches {
				found := false
				for _, gitlabBranch := range gitlabBranches {
					if githubBranch.Name != nil && *githubBranch.Name == gitlabBranch {
						found = true
						break
					}
				}
				if !found {
					refSpecsToDelete = append(refSpecsToDelete, gitconfig.RefSpec(fmt.Sprintf(":refs/heads/%s", *githubBranch.Name)))
				}
			}

			batches := ChunkRefSpecs(refSpecsToDelete, p.pushBatchSize)
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
						return p.result, formatPushError(fmt.Sprintf("trimming branch batch %d/%d", batchNum+1, len(batches)), "", err, sideband)
					}
				}
			}
		}

		p.log.Debug(pushMode+" tags to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		tagOpts := &git.PushOptions{
			RemoteName: "github",
			Force:      !p.m.Cfg.NoForce,
			RefSpecs:   []gitconfig.RefSpec{"refs/tags/*:refs/tags/*"},
		}
		tagSideband, err := p.pushWithSideband(ctx, tagOpts)
		if err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				p.log.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
			} else {
				hint := pushErrHint(err)
				if p.m.Cfg.NoForce {
					hint = " (hint: remove -no-force if push is rejected due to conflicts)" + hint
				}
				return p.result, formatPushError("pushing tags to github repo", hint, err, tagSideband)
			}
		}

		p.log.Debug("setting default repository branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "branch_name", p.defaultBranch)
		updateRepoDefault := gogithub.Repository{
			DefaultBranch: &p.defaultBranch,
		}
		if _, _, err = p.m.GH.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepoDefault); err != nil {
			return p.result, fmt.Errorf("setting default branch: %w", err)
		}
	} // end else !pullRequestsOnly

	if p.m.Cfg.EnablePullRequests {
		mrResults := p.migrateMergeRequests(ctx)
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

	p.result.EndTime = time.Now()
	p.result.Duration = p.result.EndTime.Sub(p.result.StartTime)
	p.result.BranchCount = len(p.result.BranchesMigrated)

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

func (p *project) migrateMergeRequests(ctx context.Context) []MergeRequestResult {
	var mergeRequests []*gogitlab.MergeRequest

	opts := &gogitlab.ListProjectMergeRequestsOptions{
		OrderBy: Pointer("created_at"),
		Sort:    Pointer("asc"),
	}

	if p.m.Cfg.MergeRequestsAge > 0 {
		opts.CreatedAfter = Pointer(time.Now().AddDate(0, 0, -p.m.Cfg.MergeRequestsAge))
	}

	p.log.Debug("retrieving GitLab merge requests", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID)
	for {
		result, resp, err := p.m.GL.MergeRequests.ListProjectMergeRequests(p.project.ID, opts)
		if err != nil {
			p.log.Error("retrieving gitlab merge requests", "error", err)
			return []MergeRequestResult{}
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	results := make([]MergeRequestResult, 0, len(mergeRequests))
	p.log.Info("migrating merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", len(mergeRequests))

	for _, mergeRequest := range mergeRequests {
		if mergeRequest == nil {
			continue
		}

		if p.m.Cfg.SkipOpenMergeRequests && strings.EqualFold(mergeRequest.State, "opened") {
			results = append(results, MergeRequestResult{
				GitLabMRID:    mergeRequest.IID,
				GitLabMRTitle: mergeRequest.Title,
				GitLabState:   mergeRequest.State,
				Status:        StatusSkipped,
				SkipReason:    "open merge request skipped (-skip-open-merge-requests)",
			})
			continue
		}

		mrResult, err := p.migrateMergeRequest(ctx, mergeRequest)
		if err != nil {
			p.log.Error("migrating merge request", "merge_request_id", mergeRequest.IID, "error", err)
			mrResult.Status = StatusFailed
			mrResult.Error = err.Error()
		}
		results = append(results, mrResult)
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

	return results
}

func (p *project) migrateMergeRequest(ctx context.Context, mergeRequest *gogitlab.MergeRequest) (MergeRequestResult, error) {
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

	p.log.Debug("searching for any existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID, "state", mergeRequest.State, "source_branch", mergeRequest.SourceBranch)
	sourceBranches := []string{mergeRequest.SourceBranch, sourceBranchForClosedMergeRequest}
	branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
	query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", p.githubPath[0], p.githubPath[1], branchQuery)
	searchResult, err := p.m.GHClient.GetSearchResults(ctx, query)
	if err != nil {
		return result, fmt.Errorf("listing pull requests: %w", err)
	}

	for _, issue := range searchResult.Issues {
		if issue == nil {
			continue
		}

		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("preparing to retrieve pull request: %w", err)
		}

		if issue.IsPullRequest() {
			prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
			if err != nil {
				return result, fmt.Errorf("parsing pull request url: %w", err)
			}

			if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
				prNumber, _ := strconv.Atoi(m[1])
				pr, err := p.m.GHClient.GetPullRequest(ctx, p.githubPath[0], p.githubPath[1], prNumber)
				if err != nil {
					return result, fmt.Errorf("retrieving pull request: %w", err)
				}

				if strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | %d", mergeRequest.IID)) ||
					strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | [%d]", mergeRequest.IID)) {
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
			if errors.Is(err, plumbing.ErrReferenceNotFound) && p.m.Cfg.SkipInvalidMergeRequests {
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

	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		p.log.Trace("searching for existing branch for closed/merged merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

		mergeRequest.SourceBranch = sourceBranchForClosedMergeRequest
		mergeRequest.TargetBranch = targetBranchForClosedMergeRequest

		p.log.Trace("retrieving commits for merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
		mergeRequestCommits, _, err := p.m.GL.MergeRequests.GetMergeRequestCommits(p.project.ID, mergeRequest.IID, &gogitlab.GetMergeRequestCommitsOptions{OrderBy: "created_at", Sort: "asc"})
		if err != nil {
			return result, fmt.Errorf("retrieving merge request commits: %w", err)
		}

		if len(mergeRequestCommits) == 0 {
			result.Status = StatusSkipped
			result.SkipReason = "merge request has no commits"
			return result, nil
		}

		sort.Slice(mergeRequestCommits, func(i, j int) bool {
			return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate)
		})

		if mergeRequestCommits[0] == nil {
			return result, fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
		}
		if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
			return result, fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
		}

		if p.m.Cfg.PullRequestsOnly {
			skipped, err := p.createTempBranchesViaAPI(ctx, mergeRequest, mergeRequestCommits, &result)
			if err != nil {
				return result, err
			}
			if skipped {
				return result, nil
			}
			defer func() {
				if pullRequest == nil {
					return
				}
				p.log.Debug("deleting temporary branches for closed pull request via API", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				p.deleteTempBranchesViaAPI(ctx, mergeRequest)
			}()
		} else {
			worktree, err := p.repo.Worktree()
			if err != nil {
				return result, fmt.Errorf("creating worktree: %w", err)
			}

			p.log.Trace("inspecting start commit", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
			startCommit, err := object.GetCommit(p.repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
			if err != nil {
				if p.m.Cfg.SkipInvalidMergeRequests {
					p.log.Info("skipping invalid merge request as start commit does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "missing_commit", mergeRequestCommits[0].ShortID, "error", err)
					result.Status = StatusSkipped
					result.SkipReason = "start commit does not exist"
					return result, nil
				}
				return result, fmt.Errorf("loading start commit %s: %w", mergeRequestCommits[0].ShortID, err)
			}

			if startCommit.NumParents() == 0 {
				if p.m.Cfg.SkipInvalidMergeRequests {
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
				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: true,
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(mergeRequest.TargetBranch),
					Hash:   startCommitParent.Hash,
				}); err != nil {
					return result, fmt.Errorf("checking out temporary target branch: %w", err)
				}
			}

			endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
			p.log.Trace("creating source branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)

			if _, err = object.GetCommit(p.repo.Storer, endHash); err != nil {
				if p.m.Cfg.SkipInvalidMergeRequests {
					p.log.Info("skipping invalid merge request as end commit does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "missing_commit", mergeRequestCommits[len(mergeRequestCommits)-1].ShortID, "error", err)
					result.Status = StatusSkipped
					result.SkipReason = "end commit does not exist"
					return result, nil
				}
				return result, fmt.Errorf("loading end commit %s: %w", mergeRequestCommits[len(mergeRequestCommits)-1].ShortID, err)
			}

			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: true,
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
				Hash:   endHash,
			}); err != nil {
				return result, fmt.Errorf("checking out temporary source branch: %w", err)
			}

			p.log.Debug("pushing branches for merged/closed merge request", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			mrPushOpts := &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []gitconfig.RefSpec{
					gitconfig.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
					gitconfig.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
				},
				Force: !p.m.Cfg.NoForce,
			}
			mrSideband, err := p.pushWithSideband(ctx, mrPushOpts)
			if err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					p.log.Trace("branch already exists and is up-to-date on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				} else {
					hint := pushErrHint(err)
					if p.m.Cfg.NoForce {
						hint = " (hint: remove -no-force if push is rejected due to conflicts)" + hint
					}
					return result, formatPushError("pushing temporary branches to github", hint, err, mrSideband)
				}
			}

			defer func() {
				if pullRequest == nil {
					return
				}
				p.log.Debug("deleting temporary branches for closed pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				cleanupOpts := &git.PushOptions{
					RemoteName: "github",
					RefSpecs: []gitconfig.RefSpec{
						gitconfig.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
						gitconfig.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
					},
					Force: true,
				}
				sideband, err := p.pushWithSideband(ctx, cleanupOpts)
				if err != nil {
					if errors.Is(err, git.NoErrAlreadyUpToDate) {
						p.log.Trace("branches already deleted on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
					} else {
						p.log.Error(formatPushError("pushing branch deletions to github", "", err, sideband).Error())
					}
				}
			}()
		}
	}

	if p.defaultBranch != p.project.DefaultBranch && mergeRequest.TargetBranch == p.project.DefaultBranch {
		p.log.Trace("changing target trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "old_trunk", p.project.DefaultBranch, "new_trunk", p.defaultBranch)
		mergeRequest.TargetBranch = p.defaultBranch
	}

	githubAuthorName := mergeRequest.Author.Name

	author, err := p.m.GLClient.GetUser(mergeRequest.Author.Username)
	if err != nil {
		return result, fmt.Errorf("retrieving gitlab user: %w", err)
	}
	if author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	originalState := ""
	if !strings.EqualFold(mergeRequest.State, "opened") {
		originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
	}

	p.log.Debug("determining merge request approvers", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	approvers := make([]string, 0)
	awards, _, err := p.m.GL.AwardEmoji.ListMergeRequestAwardEmoji(p.project.ID, mergeRequest.IID, &gogitlab.ListAwardEmojiOptions{PerPage: 100})
	if err != nil {
		p.log.Error("listing merge request awards", "error", err)
	} else {
		for _, award := range awards {
			if award.Name == "thumbsup" {
				approver := award.User.Name

				approverUser, err := p.m.GLClient.GetUser(award.User.Username)
				if err != nil {
					p.log.Error("retrieving gitlab user for approver", "username", award.User.Username, "error", err)
					continue
				}
				if approverUser.WebsiteURL != "" {
					approver = "@" + strings.TrimPrefix(strings.ToLower(approverUser.WebsiteURL), "https://github.com/")
				}

				approvers = append(approvers, approver)
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

	mergeRequestTitle := mergeRequest.Title
	if len(mergeRequestTitle) > 40 {
		mergeRequestTitle = mergeRequestTitle[:40] + "..."
	}

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

%[3]s`, githubAuthorName, mergeRequest.IID, description, p.gitlabPath[0], p.gitlabPath[1], mergeRequest.CreatedAt.Format(config.DateFormat), closeDate, approval, originalState, p.m.Cfg.GitlabDomain, mergeRequestTitle)

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
		if pullRequest, _, err = p.m.GH.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest); err != nil {
			if strings.Contains(err.Error(), "No commits between") {
				p.log.Debug("skipping merge request as the change is already present in trunk branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
				result.Status = StatusSkipped
				result.SkipReason = fmt.Sprintf("branch '%s' has no new commits relative to '%s'; changes are already present in the target branch", mergeRequest.SourceBranch, mergeRequest.TargetBranch)
				return result, nil
			}
			return result, fmt.Errorf("creating pull request: %w", err)
		}

		result.GitHubPRNumber = pullRequest.Number

		if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
			p.log.Debug("closing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.State = Pointer("closed")
			if pullRequest, _, err = p.m.GH.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return result, fmt.Errorf("updating pull request: %w", err)
			}
		}

	} else {
		var newState *string
		switch mergeRequest.State {
		case "opened":
			newState = Pointer("open")
		case "closed", "merged":
			newState = Pointer("closed")
		}

		if pullRequest.State != nil && newState != nil && *pullRequest.State != *newState {
			pullRequestState := &gogithub.PullRequest{
				Number: pullRequest.Number,
				State:  newState,
			}

			if pullRequest, _, err = p.m.GH.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequestState.GetNumber(), pullRequestState); err != nil {
				return result, fmt.Errorf("updating pull request state: %w", err)
			}
		}

		if (newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState)) ||
			(pullRequest.Title == nil || *pullRequest.Title != mergeRequest.Title) ||
			(pullRequest.Body == nil || *pullRequest.Body != body) ||
			(pullRequest.Draft == nil || *pullRequest.Draft != mergeRequest.Draft) {
			p.log.Info("updating pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.Title = &mergeRequest.Title
			pullRequest.Body = &body
			pullRequest.Draft = &mergeRequest.Draft
			pullRequest.MaintainerCanModify = nil
			if pullRequest, _, err = p.m.GH.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return result, fmt.Errorf("updating pull request: %w", err)
			}
		} else {
			p.log.Trace("existing pull request is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	var comments []*gogitlab.Note
	noteOpts := &gogitlab.ListMergeRequestNotesOptions{
		OrderBy: Pointer("created_at"),
		Sort:    Pointer("asc"),
	}

	p.log.Debug("retrieving GitLab merge request comments", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	for {
		notes, resp, err := p.m.GL.Notes.ListMergeRequestNotes(p.project.ID, mergeRequest.IID, noteOpts)
		if err != nil {
			return result, fmt.Errorf("listing merge request notes: %w", err)
		}

		comments = append(comments, notes...)

		if resp.NextPage == 0 {
			break
		}

		noteOpts.Page = resp.NextPage
	}

	result.TotalComments = len(comments)

	p.log.Debug("retrieving GitHub pull request comments", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
	prComments, _, err := p.m.GH.Issues.ListComments(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &gogithub.IssueListCommentsOptions{Sort: Pointer("created"), Direction: Pointer("asc")})
	if err != nil {
		p.log.Error("listing pull request comments", "error", err)
	} else {
		p.log.Info("migrating merge request comments from GitLab to GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(comments))

		for _, comment := range comments {
			if comment == nil || comment.System {
				continue
			}

			commentResult := CommentResult{
				GitLabNoteID:   comment.ID,
				AuthorUsername: comment.Author.Username,
				CreatedAt:      *comment.CreatedAt,
			}

			githubCommentAuthorName := comment.Author.Name

			commentAuthor, err := p.m.GLClient.GetUser(comment.Author.Username)
			if err != nil {
				commentResult.Status = StatusFailed
				commentResult.Error = fmt.Sprintf("retrieving gitlab user: %v", err)
				result.Comments = append(result.Comments, commentResult)
				result.FailedComments++
				p.log.Error("retrieving gitlab user for comment", "comment_id", comment.ID, "error", err)
				continue
			}
			if commentAuthor.WebsiteURL != "" {
				githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
			}

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

## Original Comment

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format("Mon, 2 Jan 2006"), comment.Body)

			foundExistingComment := false
			for _, prComment := range prComments {
				if prComment == nil {
					continue
				}

				if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
					foundExistingComment = true

					if prComment.Body == nil || *prComment.Body != commentBody {
						p.log.Debug("updating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
						prComment.Body = &commentBody
						if _, _, err = p.m.GH.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], prComment.GetID(), prComment); err != nil {
							commentResult.Status = StatusFailed
							commentResult.Error = fmt.Sprintf("updating comment: %v", err)
							result.Comments = append(result.Comments, commentResult)
							result.FailedComments++
							p.log.Error("updating pull request comment", "comment_id", comment.ID, "error", err)
							continue
						}
					}
					commentResult.Status = StatusSuccess
					commentResult.GitHubCommentID = Pointer(prComment.GetID())
					result.MigratedComments++
				} else {
					p.log.Trace("existing pull request comment is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
				}
			}

			if !foundExistingComment {
				p.log.Debug("creating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
				newComment := gogithub.IssueComment{
					Body: &commentBody,
				}
				createdComment, _, err := p.m.GH.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment)
				if err != nil {
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

	if result.FailedComments > 0 {
		result.Status = StatusPartial
	} else {
		result.Status = StatusSuccess
	}

	return result, nil
}

func (p *project) createTempBranchesViaAPI(ctx context.Context, mr *gogitlab.MergeRequest, commits []*gogitlab.Commit, result *MergeRequestResult) (bool, error) {
	owner := p.githubPath[0]
	repo := p.githubPath[1]
	startShortID := commits[0].ShortID
	endShortID := commits[len(commits)-1].ShortID

	p.log.Trace("inspecting start commit via GitHub API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "sha", startShortID)
	startCommit, resp, err := p.m.GH.Git.GetCommit(ctx, owner, repo, commits[0].ID)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			if p.m.Cfg.SkipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as start commit does not exist on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "missing_commit", startShortID)
				result.Status = StatusSkipped
				result.SkipReason = "start commit does not exist on GitHub"
				return true, nil
			}
		}
		return false, fmt.Errorf("loading start commit %s from GitHub: %w", startShortID, err)
	}

	if len(startCommit.Parents) == 0 {
		if p.m.Cfg.SkipInvalidMergeRequests {
			p.log.Info("skipping invalid merge request as start commit has no parents", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "sha", startShortID)
			result.Status = StatusSkipped
			result.SkipReason = "start commit has no parents (orphaned)"
			return true, nil
		}
		return false, fmt.Errorf("start commit %s for merge request %d has no parents", startShortID, mr.IID)
	}

	parentSHA := startCommit.Parents[0].GetSHA()
	p.log.Trace("creating target branch for merged/closed merge request via API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "branch", mr.TargetBranch, "sha", parentSHA)
	if _, _, err = p.m.GH.Git.CreateRef(ctx, owner, repo, gogithub.CreateRef{Ref: "refs/heads/" + mr.TargetBranch, SHA: parentSHA}); err != nil {
		if !isAlreadyExistsError(err) {
			return false, fmt.Errorf("creating temporary target branch %s on GitHub: %w", mr.TargetBranch, err)
		}
		p.log.Trace("temporary target branch already exists on GitHub", "branch", mr.TargetBranch)
	}

	p.log.Trace("inspecting end commit via GitHub API", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mr.IID, "sha", endShortID)
	if _, resp, err = p.m.GH.Git.GetCommit(ctx, owner, repo, commits[len(commits)-1].ID); err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			if p.m.Cfg.SkipInvalidMergeRequests {
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
	if _, _, err = p.m.GH.Git.CreateRef(ctx, owner, repo, gogithub.CreateRef{Ref: "refs/heads/" + mr.SourceBranch, SHA: endSHA}); err != nil {
		if !isAlreadyExistsError(err) {
			return false, fmt.Errorf("creating temporary source branch %s on GitHub: %w", mr.SourceBranch, err)
		}
		p.log.Trace("temporary source branch already exists on GitHub", "branch", mr.SourceBranch)
	}

	return false, nil
}

func (p *project) deleteTempBranchesViaAPI(ctx context.Context, mr *gogitlab.MergeRequest) {
	owner := p.githubPath[0]
	repo := p.githubPath[1]
	if _, err := p.m.GH.Git.DeleteRef(ctx, owner, repo, "refs/heads/"+mr.SourceBranch); err != nil {
		p.log.Warn("failed to delete temporary source branch via API", "branch", mr.SourceBranch, "error", err)
	}
	if _, err := p.m.GH.Git.DeleteRef(ctx, owner, repo, "refs/heads/"+mr.TargetBranch); err != nil {
		p.log.Warn("failed to delete temporary target branch via API", "branch", mr.TargetBranch, "error", err)
	}
}

func isAlreadyExistsError(err error) bool {
	var ghErr *gogithub.ErrorResponse
	if errors.As(err, &ghErr) {
		return ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity &&
			strings.Contains(ghErr.Message, "Reference already exists")
	}
	return false
}
