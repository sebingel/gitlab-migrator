#!/bin/bash
# GitLab to GitHub Migration Script
# Uses projects.csv to migrate multiple projects

# Configuration
GITHUB_USER="sebingel"
GITLAB_DOMAIN="gitlab.jtl-software.com"
GITHUB_DOMAIN="github.com"  # Change to your GitHub Enterprise domain if needed
PROJECTS_CSV="projects.csv"
LOG_DIRECTORY=""  # Leave empty for default (./logs next to executable)

# Optional: Set log level (ERROR, WARN, INFO, DEBUG, TRACE)
export LOG_LEVEL="TRACE"

# Required: Set authentication tokens
# Uncomment and set these, or set them in your environment before running
# export GITHUB_TOKEN="github_pat_..."
# export GITLAB_TOKEN="glpat-..."

# ============================================================================
# Command arguments
# ============================================================================
# All flags of gitlab-migrator are listed below, grouped by topic.
# To use a flag, remove the "#" at the start of its line. To stop, add it again.
# The rules between flags use these words:
#   Requires X        the tool stops with an error if X is not set
#   Excludes X        the tool stops with an error if X is also set
#   Implies X         the flag turns X on by itself
#   Only with X       without X, the flag has no effect
#   No effect with X  with X, the flag has no effect
# A flag without effect is ignored, but a bad value in it can still stop the tool.
# Active flags in this script: the deleting options (-delete-existing-repos,
# -trim-branches-on-github) are off, and no active flag needs a flag that is off.
# Check this again when you turn a flag on or off.
arguments=()

# ----------------------------------------------------------------------------
# Connection (values from the Configuration block at the top, do not leave them empty)
# ----------------------------------------------------------------------------

# -github-user: GitHub user that owns GITHUB_TOKEN. Used as the user name for git pushes.
#   Required. Migrated pull requests are created by the owner of the token.
arguments+=("-github-user" "$GITHUB_USER")

# -gitlab-domain: GitLab host (default: gitlab.com).
arguments+=("-gitlab-domain" "$GITLAB_DOMAIN")

# -github-domain: GitHub host (default: github.com). Another host means GitHub Enterprise.
arguments+=("-github-domain" "$GITHUB_DOMAIN")

# ----------------------------------------------------------------------------
# Projects to migrate
# ----------------------------------------------------------------------------
# Use -projects-csv, or both -gitlab-project and -github-repo.
# Without -projects-csv, the tool stops with an error if one of the pair is missing.
# With -projects-csv, a single one of the pair is ignored.

# -projects-csv: CSV file, one "gitlab-group/project,github-org/repo" per line, no header.
#   Excludes: -gitlab-project together with -github-repo.
arguments+=("-projects-csv" "$PROJECTS_CSV")

# -gitlab-project and -github-repo: Migrate one project. Set both of them.
#   Excludes: -projects-csv.
# arguments+=("-gitlab-project" "namespace/project")
# arguments+=("-github-repo" "org/repo")

# ----------------------------------------------------------------------------
# GitHub repository: creation and settings
# ----------------------------------------------------------------------------

# -delete-existing-repos: Delete an existing GitHub repo and create it again. It does not ask!
#   With -loop, this happens again in every pass. With -state-dir, see the note there.
#   Excludes: -pull-requests-only.
# arguments+=("-delete-existing-repos")

# -repo-visibility: private (default), internal or public, for repos the tool creates
#   (also after -delete-existing-repos). "internal" needs a GitHub Enterprise organization.
#   An existing repo keeps its visibility.
#   No effect with: -pull-requests-only (it never creates a repo).
# arguments+=("-repo-visibility" "internal")

# -unarchive-archived-repos: Unarchive an archived GitHub repo for the run, archive it again after.
#   Without it, the migration of an archived repo fails, possibly only after long API retries.
#   If you stop the run with Ctrl+C, the repo can stay unarchived.
#   No effect with: -delete-existing-repos (the new repo is not archived).
arguments+=("-unarchive-archived-repos")

# ----------------------------------------------------------------------------
# Git push and branches
# ----------------------------------------------------------------------------

# -no-force: Push without force. Use it when work has already started in the GitHub repo.
#   Also applies to the temporary branches of merged and closed merge requests.
#   No effect with: -pull-requests-only (it does not push with git).
arguments+=("-no-force")

# -push-batch-size: Branches per push (default: all at once). Try 50 to 100 for large repos.
#   Must be greater than 0. Also used by -trim-branches-on-github.
#   No effect with: -pull-requests-only.
# arguments+=("-push-batch-size" "100")

# -trim-branches-on-github: Delete GitHub branches that do not exist in GitLab.
#   Keep it off unless GitLab is the only source: it deletes also with -no-force, so it can
#   remove work that exists only on GitHub.
#   With a rename flag below, it also tries to delete the old default branch on GitHub.
#   If that is still the default branch there, GitHub can refuse and the project fails.
#   Excludes: -pull-requests-only.
# arguments+=("-trim-branches-on-github")

# -rename-master-to-main: Rename the GitLab default branch (for example master) to main on GitHub.
#   Same as -rename-trunk-branch "main".
#   Excludes: -rename-trunk-branch, -pull-requests-only.
# arguments+=("-rename-master-to-main")

# -rename-trunk-branch: Rename the GitLab default branch to this name on GitHub.
#   With -migrate-pull-requests, new pull requests for open merge requests into the old
#   branch target the new one. Pull requests from an earlier run keep their base branch.
#   Excludes: -rename-master-to-main, -pull-requests-only.
# arguments+=("-rename-trunk-branch" "main")

# ----------------------------------------------------------------------------
# Storage of the local clone
# ----------------------------------------------------------------------------

# -storage-type: memory (default) or filesystem. Use filesystem for repos too big for memory.
#   filesystem fails for projects in a GitLab subgroup (group/subgroup/project).
#   No effect with: -pull-requests-only (there is no clone).
# arguments+=("-storage-type" "filesystem")

# -storage-dir: Directory for the filesystem clone (default: the temp directory of the system).
#   The directory must already exist. The clone is deleted after each project.
#   Only with: -storage-type filesystem.
# arguments+=("-storage-dir" "/tmp/migration")

# ----------------------------------------------------------------------------
# Merge requests to pull requests
# ----------------------------------------------------------------------------

# -migrate-pull-requests: Migrate GitLab merge requests (open, merged, closed) as pull requests.
arguments+=("-migrate-pull-requests")

# -pull-requests-only: Migrate only merged and closed merge requests. No clone and no push.
#   The GitHub repo must already exist.
#   Implies: -migrate-pull-requests, -skip-open-merge-requests.
#   Excludes: -delete-existing-repos, -trim-branches-on-github, -rename-master-to-main,
#   -rename-trunk-branch.
# arguments+=("-pull-requests-only")

# -skip-open-merge-requests: Skip open merge requests. Only merged and closed ones are migrated.
#   Only with: -migrate-pull-requests, or -report (then it lowers the count).
arguments+=("-skip-open-merge-requests")

# -skip-invalid-merge-requests: Log and skip broken merge requests (for example a missing
#   branch or commit) instead of counting them as failed.
#   Only with: -migrate-pull-requests.
arguments+=("-skip-invalid-merge-requests")

# -merge-requests-max-age: Only merge requests created in the last N days.
#   Must be a whole number. 0 or less means no limit.
#   Only with: -migrate-pull-requests, or -report (then it lowers the count).
# arguments+=("-merge-requests-max-age" "365")

# ----------------------------------------------------------------------------
# State and resume
# ----------------------------------------------------------------------------

# -state-dir: Save the progress of each merge request in a JSON file per project.
#   A new run with the same directory skips merge requests that an earlier run migrated
#   or skipped (for example as invalid). Failed and partly migrated ones are tried again.
#   This also skips merge requests that were open when they were migrated, so their pull
#   requests are not updated or closed later. -skip-open-merge-requests avoids this.
#   With -delete-existing-repos, the new repo gets no pull requests for these merge
#   requests: use an empty directory then.
#   Only with: -migrate-pull-requests.
arguments+=("-state-dir" "./state")

# ----------------------------------------------------------------------------
# Run control and reports
# ----------------------------------------------------------------------------

# -max-concurrency: Number of projects migrated at the same time (default: 4).
#   Use 1 or more. The tool rejects 0 or less with an error before it starts, also with -report.
#   No effect with: -report.
# arguments+=("-max-concurrency" "8")

# -loop: After the last project, start again with the first one, until you press Ctrl+C.
#   A new pass starts only when every project of the last pass is done, so passes do
#   not overlap.
#   No effect with: -report.
# arguments+=("-loop")

# -report: Only count the merge requests of each project. Nothing is migrated or changed.
#   Most other flags have no effect then. -skip-open-merge-requests lowers the count.
# arguments+=("-report")

# -detailed-report: After the run, write a JSON and a Markdown report to the reports folder
#   next to the executable.
#   With -loop, also after each complete pass, with all results so far.
#   No effect with: -report.
arguments+=("-detailed-report")

# ----------------------------------------------------------------------------
# Logging (the log level is LOG_LEVEL at the top of this script)
# ----------------------------------------------------------------------------

# -log-output: console (default), file, or console,file.
arguments+=("-log-output" "console,file")

# -log-directory: Directory for log files (default: the logs folder next to the executable).
#   Set it with the log directory variable at the top. It is added only if it is not empty.
#   Requires: -log-output with "file".
if [ -n "$LOG_DIRECTORY" ]; then
    arguments+=("-log-directory" "$LOG_DIRECTORY")
fi

# ----------------------------------------------------------------------------
# Other
# ----------------------------------------------------------------------------

# -config: JSON file with settings. Its values override the flags of this script,
#   except -merge-requests-max-age (the flag wins) and the flags that -pull-requests-only
#   implies (they stay on).
#   Tokens are not allowed in it. They come from the environment only.
# arguments+=("-config" "migration.json")

# -version: Print the version and exit without migrating. The other flags are only parsed:
#   an unknown flag, or text for -max-concurrency, -push-batch-size or -prepare-batch-count,
#   is still an error. No other check runs.
# arguments+=("-version")

# ----------------------------------------------------------------------------
# Prepare mode (standalone): clone one repo, handle files over 100 MB, push it to a new remote
# ----------------------------------------------------------------------------
# Uncomment the block below to use it. The two lines after it (large files, batch count) are
# optional: add only what you need. The block replaces all flags above. Of those, only
# -log-output, -log-directory and -config still work in prepare mode: add them after the
# block. -version also exits before prepare mode starts.
# As in normal mode, the tool stops with an error when -log-directory is set but -log-output
# has no "file".
# Prepare mode needs no tokens: the script skips the token check when the arguments
# contain -prepare. (A -prepare set only in the -config file is not seen: the script
# then still checks the tokens.)
#
# -prepare: Start prepare mode.
#   Requires: -prepare-clone-url, -prepare-target-url.
#   Excludes: -projects-csv, -gitlab-project, -github-repo (also when set in the -config file).
# -prepare-clone-url: URL to clone from (https:// or git@).
# -prepare-target-url: URL to push to (https:// or git@).
# -prepare-large-files: remove (with git-filter-repo) or lfs (with git lfs migrate).
#   Without it, prepare mode stops when it finds a file over 100 MB.
# -prepare-batch-count: Number of push batches. Default (and 0): batches only for repos over
#   2 GiB, 10 per GiB (at least 10). A value above 0 forces batches, also for small repos.
#   A negative value is an error.
# The -prepare-* flags have no effect without -prepare.
# arguments=(
#     "-prepare"
#     "-prepare-clone-url" "https://gitlab.example.com/group/repo.git"
#     "-prepare-target-url" "https://github.com/org/repo.git"
# )
# arguments+=("-prepare-large-files" "remove")  # or "lfs"
# arguments+=("-prepare-batch-count" "10")

# Verify tokens are set (prepare mode needs no tokens)
prepare_mode=false
for arg in "${arguments[@]}"; do
    if [[ "$arg" == "-prepare" || "$arg" == "--prepare" ]]; then
        prepare_mode=true
    fi
done
if [ "$prepare_mode" = false ]; then
    if [ -z "$GITHUB_TOKEN" ]; then
        echo "Error: GITHUB_TOKEN environment variable is not set" >&2
        exit 1
    fi

    if [ -z "$GITLAB_TOKEN" ]; then
        echo "Error: GITLAB_TOKEN environment variable is not set" >&2
        exit 1
    fi
fi

# Display configuration
echo -e "\033[36mStarting GitLab to GitHub Migration\033[0m"
echo -e "\033[36m=====================================\033[0m"
echo "GitHub User:    $GITHUB_USER"
echo "GitLab Domain:  $GITLAB_DOMAIN"
echo "GitHub Domain:  $GITHUB_DOMAIN"
echo "Projects CSV:   $PROJECTS_CSV"
echo "Log Directory:  ${LOG_DIRECTORY:-(default: ./logs)}"
echo "Log Level:      $LOG_LEVEL"
echo ""
# Print each flag with its value on one line, taken from the real arguments
echo "Arguments:"
line=""
for arg in "${arguments[@]}"; do
    if [[ "$arg" == -[[:lower:]]* && -n "$line" ]]; then
        echo "  $line"
        line=""
    fi
    line="${line:+$line }$arg"
done
if [ -n "$line" ]; then
    echo "  $line"
fi
echo ""
echo -e "\033[33mPress Ctrl+C to cancel...\033[0m"
echo ""

# Run the migration
./gitlab-migrator "${arguments[@]}"

# Check exit code
exit_code=$?
# -version prints the version and exits with 0 before any migration: no success message then
show_version=false
for arg in "${arguments[@]}"; do
    if [[ "$arg" == "-version" || "$arg" == "--version" ]]; then
        show_version=true
    fi
done
if [ $exit_code -eq 0 ]; then
    if [ "$show_version" = false ]; then
        echo ""
        echo -e "\033[32mMigration completed successfully!\033[0m"
    fi
else
    echo ""
    echo -e "\033[33mMigration completed with errors (exit code: $exit_code)\033[0m"
    echo -e "\033[33mCheck log files for details\033[0m"
fi

# Pass on the exit code of the tool, so a caller or scheduler sees a failed migration
exit $exit_code
