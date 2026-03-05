package main

import (
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
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	plumbingcache "github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

func newProject(slugs []string, sType, sDir string, batchSize int) (*project, error) {
	var err error
	p := &project{
		storageType:   sType,
		storageDir:    sDir,
		pushBatchSize: batchSize,
	}
	p.log = logger.Named(slugs[0])

	p.gitlabPath, p.githubPath, err = parseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %v", err)
	}

	p.log.Info("searching for GitLab project", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	p.project, _, err = gl.Projects.GetProject(slugs[0], nil)
	if err != nil {
		return nil, fmt.Errorf("retrieving project: %v", err)
	}

	if p.project == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	p.defaultBranch = "main"
	if renameTrunkBranch != "" {
		p.defaultBranch = renameTrunkBranch
	} else if !renameMasterToMain && p.project.DefaultBranch != "" {
		p.defaultBranch = p.project.DefaultBranch
	}

	return p, nil
}

type project struct {
	log           hclog.Logger
	project       *gitlab.Project
	repo          *git.Repository
	defaultBranch string
	gitlabPath    []string
	githubPath    []string
	storageType   string        // "memory" or "filesystem"
	storageDir    string        // directory for filesystem storage
	storagePath   string        // path to cleanup for filesystem storage
	pushBatchSize int           // number of branches per push batch
	result        ProjectResult // Track migration results
}

func (p *project) createGitStorage() (storage.Storer, error) {
	if p.storageType == "filesystem" {
		var baseDir string
		if p.storageDir != "" {
			baseDir = p.storageDir
		} else {
			baseDir = os.TempDir()
		}

		// Create unique directory for this project
		tempDir, err := os.MkdirTemp(baseDir, fmt.Sprintf("gitlab-migrator-%s-%s-*", p.gitlabPath[0], p.gitlabPath[1]))
		if err != nil {
			return nil, fmt.Errorf("creating storage directory: %v", err)
		}

		p.storagePath = tempDir
		p.log.Debug("using filesystem storage", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "path", tempDir)

		gitDir := filepath.Join(tempDir, ".git")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			return nil, fmt.Errorf("creating .git directory: %v", err)
		}

		fs := osfs.New(gitDir)
		storage := filesystem.NewStorage(fs, plumbingcache.NewObjectLRUDefault())
		return storage, nil
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

// sanitizeDescription strips all control characters from a string,
// replacing them with spaces. GitHub rejects descriptions containing control chars.
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
	newRepo := github.Repository{
		Name:          pointer(p.githubPath[1]),
		Description:   &description,
		Homepage:      &homepage,
		DefaultBranch: &p.defaultBranch,
		Private:       pointer(true),
		HasIssues:     pointer(true),
		HasProjects:   pointer(true),
		HasWiki:       pointer(true),
	}
	if _, _, err := gh.Repositories.Create(ctx, p.githubPath[0], &newRepo); err != nil {
		return fmt.Errorf("creating github repo: %v", err)
	}

	return nil
}

func (p *project) migrate(ctx context.Context) (ProjectResult, error) {
	// Initialize result tracking
	p.result = ProjectResult{
		GitLabGroup:      p.gitlabPath[0],
		GitLabProject:    p.gitlabPath[1],
		GitHubOwner:      p.githubPath[0],
		GitHubRepo:       p.githubPath[1],
		StartTime:        time.Now(),
		BranchesMigrated: make([]string, 0),
		MergeRequests:    make([]MergeRequestResult, 0),
	}

	cloneUrl, err := url.Parse(p.project.HTTPURLToRepo)
	if err != nil {
		return p.result, fmt.Errorf("parsing clone URL: %v", err)
	}

	p.log.Info("mirroring repository from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_org", p.githubPath[0], "github_repo", p.githubPath[1], "force", !noForce)

	p.log.Debug("checking for existing repository on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1])
	_, _, err = gh.Repositories.Get(ctx, p.githubPath[0], p.githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return p.result, fmt.Errorf("retrieving github repo: %v", err)
	}

	homepage := fmt.Sprintf("https://%s/%s/%s", gitlabDomain, p.gitlabPath[0], p.gitlabPath[1])

	if err != nil {
		// Repository not found
		if err = p.createRepo(ctx, homepage, false); err != nil {
			return p.result, err
		}
	} else if deleteExistingRepos {
		p.log.Warn("existing repository was found on GitHub, proceeding to delete", "owner", p.githubPath[0], "repo", p.githubPath[1])
		if _, err = gh.Repositories.Delete(ctx, p.githubPath[0], p.githubPath[1]); err != nil {
			return p.result, fmt.Errorf("deleting existing github repo: %v", err)
		}

		if err = p.createRepo(ctx, homepage, true); err != nil {
			return p.result, err
		}
	}

	p.log.Debug("updating repository settings", "owner", p.githubPath[0], "repo", p.githubPath[1])
	description := sanitizeDescription(p.project.Description)
	updateRepo := github.Repository{
		Name:              pointer(p.githubPath[1]),
		Description:       &description,
		Homepage:          &homepage,
		AllowAutoMerge:    pointer(true),
		AllowMergeCommit:  pointer(true),
		AllowRebaseMerge:  pointer(true),
		AllowSquashMerge:  pointer(true),
		AllowUpdateBranch: pointer(true),
	}
	if _, _, err = gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return p.result, fmt.Errorf("updating github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// Create git storage (memory or filesystem based on configuration)
	storage, err := p.createGitStorage()
	if err != nil {
		return p.result, fmt.Errorf("creating git storage: %v", err)
	}

	// Ensure cleanup on exit
	defer p.cleanupStorage()

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	p.log.Debug("cloning repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", p.project.HTTPURLToRepo)
	p.repo, err = git.CloneContext(ctx, storage, fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		return p.result, fmt.Errorf("cloning gitlab repo: %v", err)
	}

	if p.defaultBranch != p.project.DefaultBranch {
		if gitlabTrunk, err := p.repo.Reference(plumbing.NewBranchReferenceName(p.project.DefaultBranch), false); err == nil {
			p.log.Info("renaming trunk branch prior to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())

			p.log.Debug("creating new trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())
			githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(p.defaultBranch), gitlabTrunk.Hash())
			if err = p.repo.Storer.SetReference(githubTrunk); err != nil {
				return p.result, fmt.Errorf("creating trunk branch: %v", err)
			}

			p.log.Debug("deleting old trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "sha", gitlabTrunk.Hash())
			if err = p.repo.Storer.RemoveReference(gitlabTrunk.Name()); err != nil {
				return p.result, fmt.Errorf("deleting old trunk branch: %v", err)
			}
		}
	}

	githubUrl := fmt.Sprintf("https://%s/%s/%s", githubDomain, p.githubPath[0], p.githubPath[1])
	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, githubDomain, p.githubPath[0], p.githubPath[1])

	p.log.Debug("adding remote for GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	if _, err = p.repo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return p.result, fmt.Errorf("adding github remote: %v", err)
	}

	p.log.Debug("determining branches to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	branches, err := p.repo.Branches()
	if err != nil {
		return p.result, fmt.Errorf("retrieving branches: %v", err)
	}

	gitlabBranches := make([]string, 0)
	refSpecs := make([]config.RefSpec, 0)
	if err = branches.ForEach(func(ref *plumbing.Reference) error {
		branchName := ref.Name().Short()
		gitlabBranches = append(gitlabBranches, branchName)
		p.result.BranchesMigrated = append(p.result.BranchesMigrated, branchName)
		refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref.Name())))
		return nil
	}); err != nil {
		return p.result, fmt.Errorf("parsing branches: %v", err)
	}

	// Push branches in batches
	batches := chunkRefSpecs(refSpecs, p.pushBatchSize)
	pushMode := "force-pushing"
	if noForce {
		pushMode = "pushing"
	}
	p.log.Debug(pushMode+" branches to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "total_branches", len(refSpecs), "batches", len(batches), "batch_size", p.pushBatchSize)

	for batchNum, batch := range batches {
		p.log.Debug("pushing branch batch", "name", p.gitlabPath[1], "batch", batchNum+1, "total_batches", len(batches), "branches_in_batch", len(batch))

		if err = p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			Force:      !noForce,
			RefSpecs:   batch,
			//Prune:      true, // causes error, attempts to delete main branch
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				p.log.Debug("batch already up-to-date", "batch", batchNum+1)
			} else {
				if noForce {
					return p.result, fmt.Errorf("pushing branch batch %d/%d to github (hint: remove -no-force if push is rejected due to conflicts): %v", batchNum+1, len(batches), err)
				}
				return p.result, fmt.Errorf("pushing branch batch %d/%d to github: %v", batchNum+1, len(batches), err)
			}
		}
	}

	if trimGithubBranches {
		p.log.Debug("determining old branches to trim on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		refSpecsToDelete := make([]config.RefSpec, 0)
		githubBranches, err := getGithubBranches(ctx, p.githubPath[0], p.githubPath[1])
		if err != nil {
			return p.result, fmt.Errorf("listing branches from GitHub: %v", err)
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
				refSpecsToDelete = append(refSpecsToDelete, config.RefSpec(fmt.Sprintf(":refs/heads/%s", *githubBranch.Name)))
			}
		}

		// Trim branches in batches
		batches := chunkRefSpecs(refSpecsToDelete, p.pushBatchSize)
		p.log.Debug("trimming old branches on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "total_branches", len(refSpecsToDelete), "batches", len(batches))

		for batchNum, batch := range batches {
			p.log.Debug("trimming branch batch", "name", p.gitlabPath[1], "batch", batchNum+1, "total_batches", len(batches), "branches_in_batch", len(batch))

			if err = p.repo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				Force:      true, // force is irrelevant for delete refspecs, always set true
				RefSpecs:   batch,
				//Prune:      true, // causes error, attempts to delete main branch
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					p.log.Debug("batch already up-to-date", "batch", batchNum+1)
				} else {
					return p.result, fmt.Errorf("trimming branch batch %d/%d: %v", batchNum+1, len(batches), err)
				}
			}
		}
	}

	p.log.Debug(pushMode+" tags to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	if err = p.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      !noForce,
		RefSpecs:   []config.RefSpec{"refs/tags/*:refs/tags/*"},
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			p.log.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		} else {
			if noForce {
				return p.result, fmt.Errorf("pushing tags to github repo (hint: remove -no-force if push is rejected due to conflicts): %v", err)
			}
			return p.result, fmt.Errorf("pushing tags to github repo: %v", err)
		}
	}

	p.log.Debug("setting default repository branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "branch_name", p.defaultBranch)
	updateRepo = github.Repository{
		DefaultBranch: &p.defaultBranch,
	}
	if _, _, err = gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return p.result, fmt.Errorf("setting default branch: %v", err)
	}

	if enablePullRequests {
		mrResults := p.migrateMergeRequests(ctx)
		p.result.MergeRequests = mrResults

		// Calculate MR summary stats
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
				p.result.SuccessfulMRs++ // Partial counts as successful with warnings
			}
		}
	}

	// Finalize result
	p.result.EndTime = time.Now()
	p.result.Duration = p.result.EndTime.Sub(p.result.StartTime)
	p.result.BranchCount = len(p.result.BranchesMigrated)

	// Determine overall status
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
	var mergeRequests []*gitlab.MergeRequest

	opts := &gitlab.ListProjectMergeRequestsOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	if mergeRequestsAge > 0 {
		opts.CreatedAfter = pointer(time.Now().AddDate(0, 0, -mergeRequestsAge))
	}

	p.log.Debug("retrieving GitLab merge requests", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID)
	for {
		result, resp, err := gl.MergeRequests.ListProjectMergeRequests(p.project.ID, opts)
		if err != nil {
			sendErr(fmt.Errorf("retrieving gitlab merge requests: %v", err))
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

		mrResult, err := p.migrateMergeRequest(ctx, mergeRequest)
		if err != nil {
			sendErr(err)
			mrResult.Status = StatusFailed
			mrResult.Error = err.Error()
		}
		results = append(results, mrResult)
	}

	// Calculate summary for logging
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

func (p *project) migrateMergeRequest(ctx context.Context, mergeRequest *gitlab.MergeRequest) (MergeRequestResult, error) {
	// Initialize result
	result := MergeRequestResult{
		GitLabMRID:    mergeRequest.IID,
		GitLabMRTitle: mergeRequest.Title,
		GitLabState:   mergeRequest.State,
		SourceBranch:  mergeRequest.SourceBranch,
		TargetBranch:  mergeRequest.TargetBranch,
		Comments:      make([]CommentResult, 0),
	}

	// Check for context cancellation
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("preparing to list pull requests: %v", err)
	}

	sourceBranchForClosedMergeRequest := fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)
	targetBranchForClosedMergeRequest := fmt.Sprintf("migration-target-%d/%s", mergeRequest.IID, mergeRequest.TargetBranch)

	var pullRequest *github.PullRequest

	p.log.Debug("searching for any existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID, "state", mergeRequest.State, "source_branch", mergeRequest.SourceBranch)
	sourceBranches := []string{mergeRequest.SourceBranch, sourceBranchForClosedMergeRequest}
	branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
	query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", p.githubPath[0], p.githubPath[1], branchQuery)
	searchResult, err := getGithubSearchResults(ctx, query)
	if err != nil {
		return result, fmt.Errorf("listing pull requests: %v", err)
	}

	// Look for an existing GitHub pull request
	for _, issue := range searchResult.Issues {
		if issue == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("preparing to retrieve pull request: %v", err)
		}

		if issue.IsPullRequest() {
			// Extract the PR number from the URL
			prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
			if err != nil {
				return result, fmt.Errorf("parsing pull request url: %v", err)
			}

			if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
				prNumber, _ := strconv.Atoi(m[1])
				pr, err := getGithubPullRequest(ctx, p.githubPath[0], p.githubPath[1], prNumber)
				if err != nil {
					return result, fmt.Errorf("retrieving pull request: %v", err)
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
		// Use Reference() instead of Branch() to properly handle branch names containing slashes
		branchRef := plumbing.NewBranchReferenceName(mergeRequest.SourceBranch)
		p.log.Debug("checking for source branch in local mirror", "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch, "ref_name", branchRef.String())

		if ref, err := p.repo.Reference(branchRef, false); err != nil {
			p.log.Debug("branch lookup failed", "merge_request_id", mergeRequest.IID, "error", err, "error_type", fmt.Sprintf("%T", err))
			if errors.Is(err, plumbing.ErrReferenceNotFound) && skipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as source branch does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)
				result.Status = StatusSkipped
				result.SkipReason = "source branch does not exist"
				return result, nil
			} else {
				return result, fmt.Errorf("checking source branch for merge request: %v", err)
			}
		} else {
			p.log.Debug("branch found successfully", "merge_request_id", mergeRequest.IID, "ref", ref.Name().String(), "hash", ref.Hash())
		}
	}

	// Proceed to create temporary branches when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		p.log.Trace("searching for existing branch for closed/merged merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

		// Create a worktree
		worktree, err := p.repo.Worktree()
		if err != nil {
			return result, fmt.Errorf("creating worktree: %v", err)
		}

		// Generate temporary branch names
		mergeRequest.SourceBranch = sourceBranchForClosedMergeRequest
		mergeRequest.TargetBranch = targetBranchForClosedMergeRequest

		p.log.Trace("retrieving commits for merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
		mergeRequestCommits, _, err := gl.MergeRequests.GetMergeRequestCommits(p.project.ID, mergeRequest.IID, &gitlab.GetMergeRequestCommitsOptions{OrderBy: "created_at", Sort: "asc"})
		if err != nil {
			return result, fmt.Errorf("retrieving merge request commits: %v", err)
		}

		// Some merge requests have no commits, disregard these
		if len(mergeRequestCommits) == 0 {
			result.Status = StatusSkipped
			result.SkipReason = "merge request has no commits"
			return result, nil
		}

		// API is buggy, ordering is not respected, so we'll reorder by commit datestamp
		sort.Slice(mergeRequestCommits, func(i, j int) bool {
			return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate)
		})

		if mergeRequestCommits[0] == nil {
			return result, fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
		}
		if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
			return result, fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
		}

		p.log.Trace("inspecting start commit", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
		startCommit, err := object.GetCommit(p.repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
		if err != nil {
			if skipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as start commit does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "missing_commit", mergeRequestCommits[0].ShortID, "error", err)
				result.Status = StatusSkipped
				result.SkipReason = "start commit does not exist"
				return result, nil
			}
			return result, fmt.Errorf("loading start commit %s: %v", mergeRequestCommits[0].ShortID, err)
		}

		if startCommit.NumParents() == 0 {
			// Orphaned commit, we cannot open a pull request as GitHub rejects it
			if skipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as start commit has no parents", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", startCommit.Hash)
				result.Status = StatusSkipped
				result.SkipReason = "start commit has no parents (orphaned)"
				return result, nil
			}

			return result, fmt.Errorf("start commit %s for merge request %d has no parents", mergeRequestCommits[0].ShortID, mergeRequest.IID)
		} else {
			// Sometimes we will be starting from a merge commit, so look for a suitable parent commit to branch out from
			var startCommitParent *object.Commit
			for i := 0; i < startCommit.NumParents(); i++ {
				p.log.Trace("inspecting start commit parent", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "parent_index", i, "sha", mergeRequestCommits[0].ShortID)
				startCommitParent, err = startCommit.Parent(i)
				if err != nil {
					// Don't return as we will keep trying to find a parent
					sendErr(fmt.Errorf("loading parent commit %d: %s", i, err))
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
				return result, fmt.Errorf("checking out temporary target branch: %v", err)
			}
		}

		endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
		p.log.Trace("creating source branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)

		// Validate that the end commit exists before attempting checkout
		if _, err = object.GetCommit(p.repo.Storer, endHash); err != nil {
			if skipInvalidMergeRequests {
				p.log.Info("skipping invalid merge request as end commit does not exist", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "missing_commit", mergeRequestCommits[len(mergeRequestCommits)-1].ShortID, "error", err)
				result.Status = StatusSkipped
				result.SkipReason = "end commit does not exist"
				return result, nil
			}
			return result, fmt.Errorf("loading end commit %s: %v", mergeRequestCommits[len(mergeRequestCommits)-1].ShortID, err)
		}

		if err = worktree.Checkout(&git.CheckoutOptions{
			Create: true,
			Force:  true,
			Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
			Hash:   endHash,
		}); err != nil {
			return result, fmt.Errorf("checking out temporary source branch: %v", err)
		}

		p.log.Debug("pushing branches for merged/closed merge request", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		if err = p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
			},
			Force: !noForce,
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				p.log.Trace("branch already exists and is up-to-date on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			} else {
				if noForce {
					return result, fmt.Errorf("pushing temporary branches to github (hint: remove -no-force if push is rejected due to conflicts): %v", err)
				}
				return result, fmt.Errorf("pushing temporary branches to github: %v", err)
			}
		}

		// We will clean up these temporary branches after configuring and closing the pull request
		defer func() {
			if pullRequest == nil {
				return
			}
			p.log.Debug("deleting temporary branches for closed pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			if err := p.repo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
				},
				Force: true, // force is irrelevant for delete refspecs, always set true
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					p.log.Trace("branches already deleted on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				} else {
					sendErr(fmt.Errorf("pushing branch deletions to github: %v", err))
				}
			}

		}()
	}

	if p.defaultBranch != p.project.DefaultBranch && mergeRequest.TargetBranch == p.project.DefaultBranch {
		p.log.Trace("changing target trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "old_trunk", p.project.DefaultBranch, "new_trunk", p.defaultBranch)
		mergeRequest.TargetBranch = p.defaultBranch
	}

	githubAuthorName := mergeRequest.Author.Name

	author, err := getGitlabUser(mergeRequest.Author.Username)
	if err != nil {
		return result, fmt.Errorf("retrieving gitlab user: %v", err)
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
	awards, _, err := gl.AwardEmoji.ListMergeRequestAwardEmoji(p.project.ID, mergeRequest.IID, &gitlab.ListAwardEmojiOptions{PerPage: 100})
	if err != nil {
		sendErr(fmt.Errorf("listing merge request awards: %v", err))
	} else {
		for _, award := range awards {
			if award.Name == "thumbsup" {
				approver := award.User.Name

				approverUser, err := getGitlabUser(award.User.Username)
				if err != nil {
					sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
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
		closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", mergeRequest.ClosedAt.Format(dateFormat))
	} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.Format(dateFormat))
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

%[3]s`, githubAuthorName, mergeRequest.IID, description, p.gitlabPath[0], p.gitlabPath[1], mergeRequest.CreatedAt.Format(dateFormat), closeDate, approval, originalState, gitlabDomain, mergeRequestTitle)

	if pullRequest == nil {
		p.log.Info("creating pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		newPullRequest := github.NewPullRequest{
			Title:               &mergeRequest.Title,
			Head:                &mergeRequest.SourceBranch,
			Base:                &mergeRequest.TargetBranch,
			Body:                &body,
			MaintainerCanModify: pointer(true),
			Draft:               &mergeRequest.Draft,
		}
		if pullRequest, _, err = gh.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest); err != nil {
			if strings.Contains(err.Error(), "No commits between") {
				p.log.Debug("skipping merge request as the change is already present in trunk branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
				result.Status = StatusSkipped
				result.SkipReason = fmt.Sprintf("branch '%s' has no new commits relative to '%s'; changes are already present in the target branch", mergeRequest.SourceBranch, mergeRequest.TargetBranch)
				return result, nil
			}
			return result, fmt.Errorf("creating pull request: %v", err)
		}

		result.GitHubPRNumber = pullRequest.Number

		if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
			p.log.Debug("closing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.State = pointer("closed")
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return result, fmt.Errorf("updating pull request: %v", err)
			}
		}

	} else {
		var newState *string
		switch mergeRequest.State {
		case "opened":
			newState = pointer("open")
		case "closed", "merged":
			newState = pointer("closed")
		}

		if pullRequest.State != nil && newState != nil && *pullRequest.State != *newState {
			pullRequestState := &github.PullRequest{
				Number: pullRequest.Number,
				State:  newState,
			}

			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequestState.GetNumber(), pullRequestState); err != nil {
				return result, fmt.Errorf("updating pull request state: %v", err)
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
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return result, fmt.Errorf("updating pull request: %v", err)
			}
		} else {
			p.log.Trace("existing pull request is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	var comments []*gitlab.Note
	opts := &gitlab.ListMergeRequestNotesOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	p.log.Debug("retrieving GitLab merge request comments", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	for {
		notes, resp, err := gl.Notes.ListMergeRequestNotes(p.project.ID, mergeRequest.IID, opts)
		if err != nil {
			return result, fmt.Errorf("listing merge request notes: %v", err)
		}

		comments = append(comments, notes...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	result.TotalComments = len(comments)

	p.log.Debug("retrieving GitHub pull request comments", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
	prComments, _, err := gh.Issues.ListComments(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
	if err != nil {
		sendErr(fmt.Errorf("listing pull request comments: %v", err))
	} else {
		p.log.Info("migrating merge request comments from GitLab to GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(comments))

		for _, comment := range comments {
			if comment == nil || comment.System {
				continue
			}

			// Track comment result
			commentResult := CommentResult{
				GitLabNoteID:   comment.ID,
				AuthorUsername: comment.Author.Username,
				CreatedAt:      *comment.CreatedAt,
			}

			githubCommentAuthorName := comment.Author.Name

			commentAuthor, err := getGitlabUser(comment.Author.Username)
			if err != nil {
				commentResult.Status = StatusFailed
				commentResult.Error = fmt.Sprintf("retrieving gitlab user: %v", err)
				result.Comments = append(result.Comments, commentResult)
				result.FailedComments++
				sendErr(fmt.Errorf("retrieving gitlab user for comment %d: %v", comment.ID, err))
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
						if _, _, err = gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], prComment.GetID(), prComment); err != nil {
							commentResult.Status = StatusFailed
							commentResult.Error = fmt.Sprintf("updating comment: %v", err)
							result.Comments = append(result.Comments, commentResult)
							result.FailedComments++
							sendErr(fmt.Errorf("updating pull request comment %d: %v", comment.ID, err))
							continue
						}
					}
					commentResult.Status = StatusSuccess
					commentResult.GitHubCommentID = pointer(prComment.GetID())
					result.MigratedComments++
				} else {
					p.log.Trace("existing pull request comment is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
				}
			}

			if !foundExistingComment {
				p.log.Debug("creating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
				newComment := github.IssueComment{
					Body: &commentBody,
				}
				createdComment, _, err := gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment)
				if err != nil {
					commentResult.Status = StatusFailed
					commentResult.Error = fmt.Sprintf("creating comment: %v", err)
					result.Comments = append(result.Comments, commentResult)
					result.FailedComments++
					sendErr(fmt.Errorf("creating pull request comment %d: %v", comment.ID, err))
					continue
				}
				commentResult.Status = StatusSuccess
				commentResult.GitHubCommentID = createdComment.ID
				result.MigratedComments++
			}

			result.Comments = append(result.Comments, commentResult)
		}
	}

	// Set final status based on failures
	if result.FailedComments > 0 {
		result.Status = StatusPartial
	} else {
		result.Status = StatusSuccess
	}

	return result, nil
}
