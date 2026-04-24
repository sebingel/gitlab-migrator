# GitLab Migrator — Project Context

## Project Overview

This is a GitLab to GitHub repository migration tool written in Go. It migrates git repositories with full history and converts GitLab merge requests into GitHub pull requests, including closed/merged ones.

## Build and Run Commands

### Building
The main package lives at `./cmd/gitlab-migrator`, so the build path must be specified explicitly:

```bash
go build -o gitlab-migrator ./cmd/gitlab-migrator
```

For Windows:
```bash
go build -o gitlab-migrator.exe ./cmd/gitlab-migrator
```

### Running
```bash
# Single project migration
gitlab-migrator -github-user=mytokenuser -gitlab-project=mygitlabuser/myproject -github-repo=mygithubuser/myrepo -migrate-pull-requests

# Batch migration with CSV file
gitlab-migrator -github-user=mytokenuser -projects-csv=projects.csv -migrate-pull-requests

# Windows PowerShell script for batch migration
./migrate.ps1
```

### Required Environment Variables
- `GITLAB_TOKEN`: GitLab personal access token
- `GITHUB_TOKEN`: GitHub personal access token
- `LOG_LEVEL` (optional): ERROR, WARN, INFO, DEBUG, or TRACE (default: INFO)

## Architecture

### Core Components

**`cmd/gitlab-migrator/`** — command-line entry point:
- `main.go`: CLI flag parsing, env-var validation, orchestrates migration process with concurrency control (default 4 workers).
- `app.go`: Sets up the retryable HTTP client and wires up GitHub/GitLab API clients.
- `prepare.go`: Standalone prepare mode (clone, clean large files, push to new remote).

**`internal/migration/`** — migration logic:
- `migration.go`: `Migrator` type, worker pool, project iteration.
- `project.go`: The `project` struct and its `migrate()` method — validates GitLab project, creates/updates the GitHub repo, mirror-pushes git history, migrates merge requests as pull requests. Also holds helpers like `createRepo`, `setArchived`, `setArchivedWithRetry`, `retryOnNotFound`.
- `helper.go`: Markdown formatting for migrated PRs/comments, metadata headers for idempotence, error classification helpers.
- `report.go`: Result aggregation and detailed report generation.
- `state.go`: Per-project JSON state file for resumable migrations (opt-in via `-state-dir`).

**`internal/clients/`** — API client wrappers:
- `github.go`: Thin `GitHubClient` interface over go-github (branches, PRs, users).
- `gitlab.go`: GitLab client wrapper.
- `cache.go`: Thread-safe in-memory cache for GitHub PRs, issues, and user profiles to reduce API calls.

**`internal/config/`**:
- `config.go`: Central `Config` struct, JSON config file loading, `Validate()` for cross-flag consistency checks.

### Key Libraries
- `github.com/google/go-github/v84`: GitHub API client
- `github.com/gofri/go-github-pagination`: Transparent pagination for go-github
- `github.com/xanzy/go-gitlab`: GitLab API client
- `github.com/go-git/go-git/v5`: Git operations
- `github.com/hashicorp/go-retryablehttp`: HTTP retries for API rate limiting

### Migration Process
1. Validates source GitLab project exists
2. Creates/verifies destination GitHub repository
3. Performs git mirror push (force push to maintain exact history)
4. Migrates merge requests as pull requests (if enabled):
   - Reconstructs temporary branches for closed MRs
   - Creates PRs with metadata headers for attribution
   - Migrates all comments with original authorship
   - Deletes temporary branches after PR creation
5. Optionally renames master→main branch

### Idempotence Strategy
- Embeds GitLab IDs in markdown headers of PRs/comments
- Searches for existing migrated items before creating duplicates
- Uses force mirror push for git repository (overwrites destination)
- Maintains in-memory cache to reduce redundant API calls

## Important Considerations

- **Destructive Operation**: Uses force mirror push by default - use `-no-force` for repos where work has already begun
- **API Rate Limits**: GitHub has strict rate limits; tool handles via retryable HTTP
- **Concurrency**: Default 4 workers, adjustable via `-max-concurrency`
- **CSV Format**: `gitlab-group/project,github-org/repo` (no headers)
- **Authentication**: Tokens via environment variables only (not CLI args)
- **Pull Request Attribution**: All PRs created by the token owner, original author shown in markdown header
