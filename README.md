# GitLab to GitHub Repository Migration Tool

This tool can migrate projects from GitLab to repositories on GitHub. It currently supports:

* migrating the git repository with full history
* migrating merge requests and translating them into pull requests, including closed/merged ones
* renaming the default branch (for example `master`) to `main` along the way

It does not support migrating issues, wikis or any other primitive at this time. PRs welcome! (Although please don't waste your time suggesting swathing changes by an LLM)

Both gitlab.com and GitLab self-hosted are supported, as well as github.com and GitHub Enterprise (latter untested).

## Installing

Download the [latest release](https://github.com/sebingel/gitlab-migrator/releases/latest) for your platform & architecture. Linux and macOS builds are `.tar.gz` archives, Windows builds are `.zip` archives, and each release has a checksums file. Alternatively,

```
go install github.com/sebingel/gitlab-migrator/cmd/gitlab-migrator@latest
```

Building from source needs Go 1.26 or newer (the `go` line of `go.mod`). The release binaries are built with the Go release in the `toolchain` line of `go.mod`, so they run on the systems that this Go release supports (see the [Go minimum requirements](https://go.dev/wiki/MinimumRequirements)). For example, the macOS binaries built with Go 1.27 need macOS 13 Ventura or later.

## Usage

_Example Usage_

```
gitlab-migrator -github-user=mytokenuser -gitlab-project=mygitlabuser/myproject -github-repo=mygithubuser/myrepo -migrate-pull-requests
```

Written in Go, this is a cross-platform CLI utility that accepts the following runtime arguments:

```
  -config string
        path to JSON configuration file (file values override command-line flags)
  -delete-existing-repos
        whether existing repositories should be deleted before migrating
  -detailed-report
        write detailed migration report to reports/ directory
  -github-domain string
        specifies the GitHub domain to use (default "github.com")
  -github-repo string
        the GitHub repository to migrate to
  -github-user string
        specifies the GitHub user to use, who will author any migrated PRs (required)
  -gitlab-domain string
        specifies the GitLab domain to use (default "gitlab.com")
  -gitlab-project string
        the GitLab project to migrate
  -log-directory string
        directory for session log files (defaults to ./logs in executable directory)
  -log-output string
        comma-separated log targets: console, file, or console,file (default: console)
  -loop
        continue migrating until canceled
  -max-concurrency int
        how many projects to migrate in parallel (must be at least 1) (default 4)
  -merge-requests-max-age string
        optional maximum age in days of merge requests to migrate
  -migrate-pull-requests
        whether pull requests should be migrated
  -no-force
        use regular push instead of force push (safe for repos where work has already begun)
  -prepare
        prepare mode: clone, clean large files, push to new remote (unattended)
  -prepare-batch-count int
        override batch count for push (default: auto-calculate 10 batches/GB)
  -prepare-clone-url string
        source repository clone URL (required with -prepare)
  -prepare-large-files string
        how to handle files >100MB: 'remove' (git-filter-repo) or 'lfs' (git lfs migrate)
  -prepare-target-url string
        target repository push URL (required with -prepare)
  -projects-csv string
        specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-repo)
  -pull-requests-only
        migrate only closed/merged merge requests as pull requests without cloning/pushing the repository; open MRs are skipped (repo must already exist on GitHub)
  -push-batch-size int
        number of branches to push per batch (default: unlimited, use smaller values like 50-100 for large repos)
  -rename-master-to-main
        rename the default branch of the GitLab project to main, whatever its name, and update pull requests (incompatible with -rename-trunk-branch)
  -rename-trunk-branch string
        specifies the new trunk branch name (incompatible with -rename-master-to-main)
  -repo-visibility string
        visibility for newly created GitHub repos: 'private', 'internal', or 'public' ('internal' requires a GitHub Enterprise organization) (default "private")
  -report
        report on primitives to be migrated instead of beginning migration
  -skip-invalid-merge-requests
        when true, will log and skip invalid merge requests instead of raising an error
  -skip-open-merge-requests
        skip open merge requests during migration (only migrate closed/merged MRs)
  -state-dir string
        directory for migration state files enabling resumption after interruption (opt-in)
  -storage-dir string
        directory for filesystem storage (only used when -storage-type=filesystem, defaults to temp directory)
  -storage-type string
        git storage type: 'memory' or 'filesystem' (use filesystem for large repositories) (default "memory")
  -trim-branches-on-github
        when true, will delete any branches on GitHub that are no longer present in GitLab
  -unarchive-archived-repos
        temporarily unarchive the GitHub target repo before migration and re-archive it afterwards; without this flag, archived repos cause hard failures
  -version
        output version information
```

## Authentication

For authentication, the `GITLAB_TOKEN` and `GITHUB_TOKEN` environment variables must be populated. You cannot specify tokens as command-line arguments.

Use the `-github-user` argument to specify the GitHub username for whom the authentication token was issued (mandatory). You can also specify this with the `GITHUB_USER` environment variable.

Specify the location of a self-hosted instance of GitLab with the `-gitlab-domain` argument, or a GitHub Enterprise instance with the `-github-domain` argument.

## Specify repositories

You can specify an individual GitLab project with the `-gitlab-project` argument, along with the target GitHub repository with the `-github-repo` argument.

Alternatively, you can supply the path to a CSV file with the `-projects-csv` argument, which should contain two columns:

```csv
gitlab-group/gitlab-project-name,github-org-or-user/github-repo-name
```

If the destination repository does not exist, this tool will attempt to create it as private by default. Use `-repo-visibility=internal` or `-repo-visibility=public` to change this (`internal` requires a GitHub Enterprise organization that supports it). If the destination repo already exists, it will be used unless you specify `-delete-existing-repos`; visibility of an already-existing repo is left untouched.

> [!WARNING]  
> To delete existing GitHub repos prior to migrating, pass the `-delete-existing-repos` argument. _This is potentially dangerous, you won't be asked for confirmation!_

## Pull requests

To enable migration of GitLab merge requests to GitHub pull requests (including closed/merged ones!), specify `-migrate-pull-requests`.

Whilst the git repository itself will be migrated verbatim, the pull requests are managed using the GitHub API and typically will be authored by the person supplying the authentication token.

Each pull request, along with every comment, will be prepended with a Markdown table showing the original author and some other metadata that is useful to know.  This is also used to map pull requests and their comments to their counterparts in GitLab and enables the tool to be idempotent.

As a bonus, if your GitLab users add the URL to their GitHub profile in the `Website` field of their GitLab profile, this tool will add a link to their GitHub profile in the markdown header of any PR or comment they originally authored.

This tool also migrates merged/closed merge requests from your GitLab projects. It does this by reconstructing temporary branches in each repo, pushing them to GitHub, creating then closing the pull request, and lastly deleting the temporary branches. Once the tool has completed, you should not have any of these temporary branches in your repo - although GitHub will not garbage collect them immediately such that you can click the `Restore branch` button in any of these PRs.

If you have a large number of merge requests, or projects with a long history spanning many GitLab upgrades, you may wish to specify the `-skip-invalid-merge-requests` argument. This will cause the tool to emit INFO messages for merge requests that it considers invalid, such as those that are still marked as Open but have no source/head branch, or where there is no diff for a closed merge request. Without this option, an error will be logged instead.

Similarly, you can specify a maximum age for merge requests to migrate with the `-merge-requests-max-age` argument, which is useful for 'topping off' projects that are already migrated.

The clone of a GitLab repository contains all refs that GitLab shows, also `refs/merge-requests/<iid>/head`. The commits of a closed or merged merge request whose source branch was deleted can still be missing, when GitLab keeps them only under refs that it hides. The tool then fetches the missing commits from GitLab by their SHA. This works only if the GitLab server allows to fetch commits that it does not show. If the fetch fails, the merge request fails with an error that names the cause, or it is skipped with `-skip-invalid-merge-requests`. A network error, a timeout, a refused login (HTTP 401 or 403) or a server error (HTTP 408, 429 or 5xx) during the fetch always fails the merge request, also with `-skip-invalid-merge-requests`, so a later run with `-state-dir` tries it again. Older versions skipped these merge requests with the reason "start commit does not exist" or "end commit does not exist". A run with `-state-dir` migrates merge requests with these saved reasons again. The fetched commits are only used for the temporary branches of the merge request. No other new ref is pushed to GitHub.

Use `-skip-open-merge-requests` to only migrate closed/merged MRs, skipping any that are still open.

If the repository is already on GitHub and you only need to backfill pull requests (e.g. after a prior migration), use `-pull-requests-only`. This skips the git clone/push step entirely (the repo must already exist on GitHub) and only migrates closed/merged merge requests.

_Example migrated pull request (open)_

![example migrated open pull request](pr-example-open.jpeg)

_Example migrated pull request (closed)_

![example migrated closed pull request](pr-example-closed.jpeg)

## Renaming the default/trunk branch

As a bonus, this tool can transparently rename the trunk branch on your GitHub repository - enable with the `-rename-trunk-branch` argument. This will also work for any open merge requests as they are translated to pull requests.

`-rename-master-to-main` is the short form of `-rename-trunk-branch main`. Despite its name, it does not look for a branch called `master`: it renames the default branch of the GitLab project to `main`, whatever its name. For example, when the default branch of the GitLab project is `develop` and there is also a branch `master`, `develop` is pushed as `main` and becomes the default branch on GitHub, `master` stays `master`, and pull requests that target `develop` get the base branch `main`. When the default branch of the GitLab project is neither `master` nor `main`, the tool logs a warning at the start of the project, for example `-rename-master-to-main: the default branch of the GitLab project is "develop", not "master"; it will be renamed to "main"`. Use `-rename-trunk-branch` to choose another name. The two arguments exclude each other.

Pull requests that exist on GitHub from an earlier run also get the new trunk as their base branch, as long as they are open. GitHub does not allow to change the base branch of a closed pull request, so closed pull requests keep their base branch. A pull request that is reopened because its merge request is open again gets the new base branch after it is reopened. With `-skip-open-merge-requests`, open merge requests are skipped, so their existing pull requests keep the old base branch, unless the trim below changes it. With `-trim-branches-on-github`, the old trunk is deleted on GitHub during the push, and GitHub closes the open pull requests whose base branch is deleted. So before the old trunk is deleted, all open pull requests on it get the new trunk as their base branch. This is also true when the trim deletes the branch that was the default branch on GitHub before the run, for example when an earlier run used a rename and this run does not. The new trunk also becomes the default branch of the GitHub repository before the old trunk is deleted, because GitHub does not allow to delete the default branch.

## Concurrency

By default, 4 workers will be spawned to migrate up to 4 projects in parallel. You can increase or decrease this with the `-max-concurrency` argument. Note that due to GitHub API rate-limiting, you may not experience any significant speed-up. See [GitHub API docs](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) for details.

Specify `-loop` to continue migrating projects until canceled. This is useful for daemonizing the migration tool, or automatically restarting when migrating a large number of projects (or a small number of very large projects). A new pass starts only when every project of the previous pass is done, so passes do not overlap.

With `-loop` and `-detailed-report`, the detailed report is written each time a pass over all projects is complete, and once more when the loop ends. Each write replaces the report files of the session in `reports/` with all results collected so far, so the results of complete passes are on disk while the tool runs.

Press Ctrl+C to stop the migration, also with `-loop`. The tool then starts no new project, lets the running projects stop, and prints the summary (and writes the reports with `-detailed-report`). If this takes too long, press Ctrl+C a second time: the tool then stops at once with exit code 130, without the summary, the reports and other cleanup.

## Logging

This tool is entirely noninteractive and outputs different levels of logs depending on your interest. You can set the `LOG_LEVEL` environment to one of `ERROR`, `WARN`, `INFO`, `DEBUG` or `TRACE` to get more or less verbosity. The default is `INFO`.

By default, logs are written to the console (stderr). Use `-log-output` to write logs to a file or both console and file simultaneously:

```
# Console only (default)
gitlab-migrator ...

# File only
gitlab-migrator -log-output=file ...

# Both console and file
gitlab-migrator -log-output=console,file ...
```

Log files are written to a `logs/` subdirectory next to the executable, with session-based filenames. Use `-log-directory` to specify a custom directory.

## Storage

By default, git repositories are cloned into memory (`-storage-type=memory`). For large repositories that exceed available RAM, use filesystem-based storage:

```
gitlab-migrator -storage-type=filesystem ...
```

By default, the temporary directory of the system is used. Specify `-storage-dir` to create the temporary directory for each project inside another directory, for example on a disk with more free space. The tool removes this temporary directory when the project is done, so `-storage-dir` does not keep anything and cannot resume a migration. To resume an interrupted migration, use `-state-dir` (see [Resuming interrupted migrations](#resuming-interrupted-migrations)).

For very large repositories with many branches, use `-push-batch-size` to push branches in smaller batches (e.g., `-push-batch-size=50`) to avoid timeouts.

## Prepare Mode

The prepare mode (`-prepare`) is an unattended workflow for pre-processing large repositories before the main migration:

```
gitlab-migrator -prepare -prepare-clone-url=https://gitlab.com/group/repo.git -prepare-target-url=https://github.com/org/repo.git
```

This clones the source repository and pushes it to the target. For repositories with files over 100MB, use `-prepare-large-files` to handle them automatically:

- `remove`: Remove large files using git-filter-repo
- `lfs`: Migrate large files to Git LFS

The `-prepare-batch-count` flag overrides the automatic batch calculation (default: 10 batches per GB).

## Caching

The tool maintains a thread-safe in-memory cache for certain primitives, in order to help reduce the number of API requests being made. At this time, the following are cached the first time they are encountered, and thereafter retrieved from the cache until the tool is restarted:

- GitHub branches
- GitHub pull requests
- GitHub issue search results
- GitLab user profiles

## Idempotence

This tool tries to be idempotent. You can run it over and over and it will patch the GitHub repository, along with its pull requests, to match what you have in GitLab. This should help you migrate a number of projects without enacting a large maintenance window.

_Note that this tool performs a forced mirror push by default, so it's not recommended to run this tool after commencing work in the target repository. Use `-no-force` if you need a regular push instead._

For pull requests and their comments, the corresponding IDs from GitLab are added to the Markdown header, this is parsed to enable idempotence (see next section).

## Configuration file

Instead of (or in addition to) command-line flags, you can supply a JSON configuration file with `-config`:

```
gitlab-migrator -config=migration.json
```

Values in the file override any matching command-line flags. Field names are `snake_case` versions of the flags (e.g. `migrate_pull_requests`, `skip_open_merge_requests`). Tokens (`github_token`, `gitlab_token`) are rejected if present in the file; they must be supplied via the `GITHUB_TOKEN`/`GITLAB_TOKEN` environment variables.

## Resuming interrupted migrations

Pass `-state-dir` with a directory path to have the tool persist per-merge-request migration state to a JSON file as it works. If a migration is interrupted, re-running with the same `-state-dir` will skip merge requests that already completed successfully, so you can safely resume large or long-running migrations without redoing completed work.

With `-rename-master-to-main` or `-rename-trunk-branch`, a merge request that the state file records as migrated, and that is open and targets the GitLab trunk, still gets the new trunk as the base branch of its pull request, if the pull request is open. Nothing else of the pull request is updated. If GitHub refuses the new base branch, the merge request counts as failed in this run, and the state file keeps it as successful, so the next run tries the base branch again. With `-skip-open-merge-requests` this step does not happen. But with `-trim-branches-on-github`, the open pull requests on the old trunk still get the new trunk as base branch before the trim deletes the old trunk (see [Renaming the default/trunk branch](#renaming-the-defaulttrunk-branch)), also with `-skip-open-merge-requests`. If GitHub refuses one of these base changes before the trim, the migration of the whole project fails: the old trunk is not deleted and no merge request is migrated in this run.

## Archived repositories

By default, attempting to migrate into an archived GitHub repository fails. Pass `-unarchive-archived-repos` to have the tool temporarily unarchive the target repository before migrating, then re-archive it afterwards. The tool also re-archives the repository when you stop the migration with Ctrl+C; after Ctrl+C it tries this for up to two minutes. A second Ctrl+C stops the tool at once, also during this time, so the repository can stay unarchived. If the re-archive fails, the log says that you must archive the repository by hand.

## Reporting

Use `-report` to get a summary of what would be migrated without actually performing the migration. For a detailed per-project breakdown written to disk, add `-detailed-report`, which generates both a JSON and a Markdown report in a `reports/` subdirectory next to the executable. With `-loop`, the report is also written after each complete pass (see [Concurrency](#concurrency)).

## Contributing, reporting bugs etc...

Please use GitHub issues & pull requests. See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow and how releases are made. This project is licensed under the MIT license.
