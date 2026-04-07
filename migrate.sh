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

# Verify tokens are set
if [ -z "$GITHUB_TOKEN" ]; then
    echo "Error: GITHUB_TOKEN environment variable is not set" >&2
    exit 1
fi

if [ -z "$GITLAB_TOKEN" ]; then
    echo "Error: GITLAB_TOKEN environment variable is not set" >&2
    exit 1
fi

# Build command arguments
arguments=(
    "-github-user" "$GITHUB_USER"
    "-gitlab-domain" "$GITLAB_DOMAIN"
    "-github-domain" "$GITHUB_DOMAIN"
    "-projects-csv" "$PROJECTS_CSV"
#     "-migrate-pull-requests"
    "-skip-invalid-merge-requests"
    "-trim-branches-on-github"
    "-log-output" "console,file"
)

# Add log directory only if explicitly set
if [ -n "$LOG_DIRECTORY" ]; then
    arguments+=("-log-directory" "$LOG_DIRECTORY")
fi

# Optional flags - uncomment as needed
# arguments+=("-delete-existing-repos")
# arguments+=("-rename-master-to-main")
# arguments+=("-loop")
# arguments+=("-max-concurrency" "8")
# arguments+=("-merge-requests-max-age" "365")
arguments+=("-detailed-report")
# arguments+=("-storage-type" "filesystem")
# arguments+=("-storage-dir" "/tmp/migration")
# arguments+=("-state-dir" "./state")
arguments+=("-no-force")
# arguments+=("-push-batch-size" "100")
# arguments+=("-skip-open-merge-requests")
# arguments+=("-pull-requests-only")
# arguments+=("-report")
# arguments+=("-rename-trunk-branch" "main")
# arguments+=("-config" "migration.json")
# arguments+=("-version")

# Prepare mode (standalone, incompatible with normal migration flags)
# arguments=(
#     "-prepare"
#     "-prepare-clone-url" "https://gitlab.example.com/group/repo.git"
#     "-prepare-target-url" "https://github.com/org/repo.git"
# )
# arguments+=("-prepare-large-files" "remove")  # or "lfs"
# arguments+=("-prepare-batch-count" "10")

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
echo "Flags:"
echo "  - Migrate Pull Requests:       Enabled"
echo "  - Skip Invalid Merge Requests: Enabled"
echo "  - Trim Branches on GitHub:     Enabled"
NO_FORCE="Disabled"
for arg in "${arguments[@]}"; do
    if [ "$arg" = "-no-force" ]; then
        NO_FORCE="Enabled"
        break
    fi
done
echo "  - No Force Push:               $NO_FORCE"
echo ""
echo -e "\033[33mPress Ctrl+C to cancel...\033[0m"
echo ""

# Run the migration
./gitlab-migrator "${arguments[@]}"

# Check exit code
exit_code=$?
if [ $exit_code -eq 0 ]; then
    echo ""
    echo -e "\033[32mMigration completed successfully!\033[0m"
else
    echo ""
    echo -e "\033[33mMigration completed with errors (exit code: $exit_code)\033[0m"
    echo -e "\033[33mCheck log files for details\033[0m"
fi
