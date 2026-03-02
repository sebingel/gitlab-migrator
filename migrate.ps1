# GitLab to GitHub Migration Script
# Uses projects.csv to migrate multiple projects

# Configuration
$GitHubUser = "sebingel"
$GitLabDomain = "gitlab.jtl-software.com"
$GitHubDomain = "github.com"  # Change to your GitHub Enterprise domain if needed
$ProjectsCsv = "projects.csv"
$LogDirectory = ""  # Leave empty for default (./logs next to executable)

# Optional: Set log level (ERROR, WARN, INFO, DEBUG, TRACE)
$env:LOG_LEVEL = "TRACE"

# Required: Set authentication tokens
# Uncomment and set these, or set them in your environment before running
# $env:GITHUB_TOKEN = "github_pat_..."
# $env:GITLAB_TOKEN = "glpat-..."

# Verify tokens are set
if (-not $env:GITHUB_TOKEN) {
    Write-Error "GITHUB_TOKEN environment variable is not set"
    exit 1
}

if (-not $env:GITLAB_TOKEN) {
    Write-Error "GITLAB_TOKEN environment variable is not set"
    exit 1
}

# Build command arguments
$arguments = @(
    "-github-user", $GitHubUser,
    "-gitlab-domain", $GitLabDomain,
    "-github-domain", $GitHubDomain,
    "-projects-csv", $ProjectsCsv,
    "-migrate-pull-requests",
    "-skip-invalid-merge-requests",
    "-trim-branches-on-github",
    "-log-output", "console,file"
)

# Add log directory only if explicitly set
if ($LogDirectory) {
    $arguments += "-log-directory", $LogDirectory
}

# Optional flags - uncomment as needed
# $arguments += "-delete-existing-repos"
# $arguments += "-rename-master-to-main"
# $arguments += "-loop"
# $arguments += "-max-concurrency", "8"
# $arguments += "-merge-requests-max-age", "365"
$arguments += "-detailed-report"
# $arguments += "-storage-type", "filesystem"
# $arguments += "-storage-dir", "C:\temp\migration"
# $arguments += "-push-batch-size", "100"
# $arguments += "-report"
# $arguments += "-rename-trunk-branch", "main"
# $arguments += "-version"

# Display configuration
Write-Host "Starting GitLab to GitHub Migration" -ForegroundColor Cyan
Write-Host "=====================================" -ForegroundColor Cyan
Write-Host "GitHub User:    $GitHubUser"
Write-Host "GitLab Domain:  $GitLabDomain"
Write-Host "GitHub Domain:  $GitHubDomain"
Write-Host "Projects CSV:   $ProjectsCsv"
Write-Host "Log Directory:  $(if ($LogDirectory) { $LogDirectory } else { '(default: ./logs)' })"
Write-Host "Log Level:      $($env:LOG_LEVEL)"
Write-Host ""
Write-Host "Flags:"
Write-Host "  - Migrate Pull Requests:       Enabled"
Write-Host "  - Skip Invalid Merge Requests: Enabled"
Write-Host "  - Trim Branches on GitHub:     Enabled"
Write-Host ""
Write-Host "Press Ctrl+C to cancel..." -ForegroundColor Yellow
Write-Host ""

# Run the migration
& .\gitlab-migrator.exe @arguments

# Check exit code
if ($LASTEXITCODE -eq 0) {
    Write-Host ""
    Write-Host "Migration completed successfully!" -ForegroundColor Green
} else {
    Write-Host ""
    Write-Host "Migration completed with errors (exit code: $LASTEXITCODE)" -ForegroundColor Yellow
    Write-Host "Check log files for details" -ForegroundColor Yellow
}

