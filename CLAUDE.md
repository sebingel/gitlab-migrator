# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a GitLab to GitHub repository migration tool written in Go. It migrates git repositories with full history and converts GitLab merge requests into GitHub pull requests, including closed/merged ones.

## Build and Run Commands

### Building
```bash
go build -o gitlab-migrator
```

For Windows:
```bash
go build -o gitlab-migrator.exe
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

**main.go**: Entry point, CLI flag parsing, orchestrates migration process with concurrency control (default 4 workers).

**project.go**: Contains the `project` struct and migration logic:
- `newProject()`: Initializes project with GitLab/GitHub paths
- `createRepo()`: Creates GitHub repository if needed
- `migrate()`: Performs the actual migration including git mirroring and PR creation

**helper.go**: Utility functions for GitHub API interactions, markdown formatting for migrated PRs/comments, and metadata preservation for idempotence.

**cache.go**: Thread-safe in-memory cache for GitHub PRs, issues, and user profiles to reduce API calls.

**search.go**: GitHub issue/PR search functionality for finding existing migrated items (enables idempotence).

### Key Libraries
- `github.com/google/go-github/v74`: GitHub API client
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