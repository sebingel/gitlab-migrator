package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ResultStatus represents the outcome of a migration operation.
type ResultStatus string

const (
	StatusSuccess ResultStatus = "success"
	StatusFailed  ResultStatus = "failed"
	StatusSkipped ResultStatus = "skipped"
	StatusPartial ResultStatus = "partial"
)

// MigrationReport is the top-level structure containing all migration results.
type MigrationReport struct {
	StartTime       time.Time       `json:"start_time"`
	EndTime         time.Time       `json:"end_time"`
	Duration        time.Duration   `json:"duration"`
	TotalProjects   int             `json:"total_projects"`
	SuccessProjects int             `json:"success_projects"`
	FailedProjects  int             `json:"failed_projects"`
	PartialProjects int             `json:"partial_projects"`
	Projects        []ProjectResult `json:"projects"`
}

// ProjectResult contains results for a single project migration.
type ProjectResult struct {
	GitLabGroup   string       `json:"gitlab_group"`
	GitLabProject string       `json:"gitlab_project"`
	GitHubOwner   string       `json:"github_owner"`
	GitHubRepo    string       `json:"github_repo"`
	Status        ResultStatus `json:"status"`
	Error         string       `json:"error,omitempty"`
	StartTime     time.Time    `json:"start_time"`
	EndTime       time.Time    `json:"end_time"`
	Duration      time.Duration `json:"duration"`

	BranchesMigrated []string `json:"branches_migrated"`
	BranchCount      int      `json:"branch_count"`

	MergeRequests []MergeRequestResult `json:"merge_requests,omitempty"`
	TotalMRs      int                  `json:"total_mrs"`
	SuccessfulMRs int                  `json:"successful_mrs"`
	FailedMRs     int                  `json:"failed_mrs"`
	SkippedMRs    int                  `json:"skipped_mrs"`
}

// MergeRequestResult contains results for a single MR migration.
type MergeRequestResult struct {
	GitLabMRID     int          `json:"gitlab_mr_id"`
	GitLabMRTitle  string       `json:"gitlab_mr_title"`
	GitLabState    string       `json:"gitlab_state"`
	SourceBranch   string       `json:"source_branch"`
	TargetBranch   string       `json:"target_branch"`
	GitHubPRNumber *int         `json:"github_pr_number,omitempty"`
	Status         ResultStatus `json:"status"`
	Error          string       `json:"error,omitempty"`
	SkipReason     string       `json:"skip_reason,omitempty"`

	Comments         []CommentResult `json:"comments,omitempty"`
	TotalComments    int             `json:"total_comments"`
	MigratedComments int             `json:"migrated_comments"`
	FailedComments   int             `json:"failed_comments"`
}

// CommentResult contains results for a single comment migration.
type CommentResult struct {
	GitLabNoteID    int          `json:"gitlab_note_id"`
	GitHubCommentID *int64       `json:"github_comment_id,omitempty"`
	Status          ResultStatus `json:"status"`
	Error           string       `json:"error,omitempty"`
	AuthorUsername  string       `json:"author_username"`
	CreatedAt       time.Time    `json:"created_at"`
}

// ResultCollector is a thread-safe collector for migration results.
type ResultCollector struct {
	mutex  *sync.RWMutex
	report *MigrationReport
}

// NewResultCollector creates a new ResultCollector.
func NewResultCollector() *ResultCollector {
	return &ResultCollector{
		mutex: new(sync.RWMutex),
		report: &MigrationReport{
			StartTime: time.Now(),
			Projects:  make([]ProjectResult, 0),
		},
	}
}

// AddProjectResult adds a project result to the collection (thread-safe).
func (rc *ResultCollector) AddProjectResult(result ProjectResult) {
	rc.mutex.Lock()
	defer rc.mutex.Unlock()

	rc.report.Projects = append(rc.report.Projects, result)
	rc.report.TotalProjects++

	switch result.Status {
	case StatusSuccess:
		rc.report.SuccessProjects++
	case StatusFailed:
		rc.report.FailedProjects++
	case StatusPartial:
		rc.report.PartialProjects++
	}
}

// Finalize finalizes the report with end time and duration.
func (rc *ResultCollector) Finalize() *MigrationReport {
	rc.mutex.Lock()
	defer rc.mutex.Unlock()

	rc.report.EndTime = time.Now()
	rc.report.Duration = rc.report.EndTime.Sub(rc.report.StartTime)

	return rc.report
}

// WriteJSONReport writes the migration report as JSON to the specified path.
func WriteJSONReport(report *MigrationReport, outputPath string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling report to JSON: %v", err)
	}
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("writing JSON report: %v", err)
	}
	return nil
}

// WriteMarkdownReport writes the migration report as Markdown to the specified path.
func WriteMarkdownReport(report *MigrationReport, outputPath string) error {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "# GitLab to GitHub Migration Report\n\n")
	fmt.Fprintf(&buf, "**Generated:** %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&buf, "**Duration:** %s\n\n", report.Duration.Round(time.Second))

	fmt.Fprintf(&buf, "## Summary\n\n")
	fmt.Fprintf(&buf, "| Metric | Count |\n")
	fmt.Fprintf(&buf, "|--------|-------|\n")
	fmt.Fprintf(&buf, "| Total Projects | %d |\n", report.TotalProjects)
	fmt.Fprintf(&buf, "| Successful | %d |\n", report.SuccessProjects)
	fmt.Fprintf(&buf, "| Failed | %d |\n", report.FailedProjects)
	fmt.Fprintf(&buf, "| Partial | %d |\n\n", report.PartialProjects)

	for _, proj := range report.Projects {
		fmt.Fprintf(&buf, "## Project: %s/%s → %s/%s\n\n",
			proj.GitLabGroup, proj.GitLabProject, proj.GitHubOwner, proj.GitHubRepo)

		fmt.Fprintf(&buf, "**Status:** %s\n", proj.Status)
		fmt.Fprintf(&buf, "**Duration:** %s\n\n", proj.Duration.Round(time.Second))

		if proj.Error != "" {
			if strings.Contains(proj.Error, "\n") {
				fmt.Fprintf(&buf, "**Error:**\n\n```\n%s\n```\n\n", proj.Error)
			} else {
				fmt.Fprintf(&buf, "**Error:** %s\n\n", proj.Error)
			}
		}

		fmt.Fprintf(&buf, "### Branches Migrated (%d)\n\n", proj.BranchCount)
		if len(proj.BranchesMigrated) > 0 {
			for _, branch := range proj.BranchesMigrated {
				fmt.Fprintf(&buf, "- `%s`\n", branch)
			}
			fmt.Fprintf(&buf, "\n")
		}

		fmt.Fprintf(&buf, "### Merge Requests\n\n")
		fmt.Fprintf(&buf, "| Metric | Count |\n")
		fmt.Fprintf(&buf, "|--------|-------|\n")
		fmt.Fprintf(&buf, "| Total | %d |\n", proj.TotalMRs)
		fmt.Fprintf(&buf, "| Successful | %d |\n", proj.SuccessfulMRs)
		fmt.Fprintf(&buf, "| Failed | %d |\n", proj.FailedMRs)
		fmt.Fprintf(&buf, "| Skipped | %d |\n\n", proj.SkippedMRs)

		if proj.FailedMRs > 0 || proj.SkippedMRs > 0 {
			fmt.Fprintf(&buf, "#### Failed/Skipped Merge Requests\n\n")
			fmt.Fprintf(&buf, "| MR ID | Title | Status | Reason/Error |\n")
			fmt.Fprintf(&buf, "|-------|-------|--------|-------------|\n")

			for _, mr := range proj.MergeRequests {
				if mr.Status == StatusFailed || mr.Status == StatusSkipped {
					reason := mr.SkipReason
					if reason == "" {
						reason = mr.Error
					}
					fmt.Fprintf(&buf, "| %d | %s | %s | %s |\n",
						mr.GitLabMRID, escapeMDCell(mr.GitLabMRTitle), mr.Status, escapeMDCell(reason))
				}
			}
			fmt.Fprintf(&buf, "\n")
		}

		failedComments := 0
		for _, mr := range proj.MergeRequests {
			failedComments += mr.FailedComments
		}

		if failedComments > 0 {
			fmt.Fprintf(&buf, "#### Comment Migration Issues\n\n")
			fmt.Fprintf(&buf, "| MR ID | Total Comments | Failed | Details |\n")
			fmt.Fprintf(&buf, "|-------|----------------|--------|--------|\n")

			for _, mr := range proj.MergeRequests {
				if mr.FailedComments > 0 {
					errs := make([]string, 0)
					for _, comment := range mr.Comments {
						if comment.Status == StatusFailed {
							errs = append(errs, fmt.Sprintf("Note %d: %s", comment.GitLabNoteID, comment.Error))
						}
					}
					fmt.Fprintf(&buf, "| %d | %d | %d | %s |\n",
						mr.GitLabMRID, mr.TotalComments, mr.FailedComments, escapeMD(strings.Join(errs, "; ")))
				}
			}
			fmt.Fprintf(&buf, "\n")
		}

		fmt.Fprintf(&buf, "---\n\n")
	}

	if err := os.WriteFile(outputPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("writing markdown report: %v", err)
	}

	return nil
}

// PrintSummaryToConsole prints a concise summary of the migration to console.
func PrintSummaryToConsole(report *MigrationReport) {
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("MIGRATION SUMMARY")
	fmt.Println(strings.Repeat("=", 80))

	fmt.Printf("Duration: %s\n", report.Duration.Round(time.Second))
	fmt.Printf("Total Projects: %d (Success: %d, Failed: %d, Partial: %d)\n\n",
		report.TotalProjects, report.SuccessProjects, report.FailedProjects, report.PartialProjects)

	for _, proj := range report.Projects {
		var statusSymbol string
		switch proj.Status {
		case StatusFailed:
			statusSymbol = "✗"
		case StatusPartial:
			statusSymbol = "⚠"
		default:
			statusSymbol = "✓"
		}

		fmt.Printf("%s %s/%s → %s/%s\n",
			statusSymbol, proj.GitLabGroup, proj.GitLabProject, proj.GitHubOwner, proj.GitHubRepo)
		fmt.Printf("  Branches: %d | MRs: %d (Success: %d, Failed: %d, Skipped: %d)\n",
			proj.BranchCount, proj.TotalMRs, proj.SuccessfulMRs, proj.FailedMRs, proj.SkippedMRs)

		if proj.Error != "" {
			indented := strings.ReplaceAll(proj.Error, "\n", "\n    ")
			fmt.Printf("  Error: %s\n", indented)
		}

		failedComments := 0
		for _, mr := range proj.MergeRequests {
			failedComments += mr.FailedComments
		}
		if failedComments > 0 {
			fmt.Printf("  Warning: %d comment(s) failed to migrate\n", failedComments)
		}

		fmt.Println()
	}

	fmt.Println(strings.Repeat("=", 80))
}

func escapeMD(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func escapeMDCell(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i] + " [...]"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}
