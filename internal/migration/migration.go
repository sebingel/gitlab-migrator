package migration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	gogitlab "github.com/xanzy/go-gitlab"
	"github.com/manicminer/gitlab-migrator/internal/clients"
	"github.com/manicminer/gitlab-migrator/internal/config"
)

// MigrationPartialError is returned when migration completes but some projects had errors.
type MigrationPartialError struct {
	FailedProjects  int
	PartialProjects int
}

func (e *MigrationPartialError) Error() string {
	return fmt.Sprintf("migration completed with %d failed and %d partial project(s), review log output for details", e.FailedProjects, e.PartialProjects)
}

// CSVRow represents one project mapping from the projects CSV.
type CSVRow = []string

// Migrator holds all runtime dependencies for the migration process.
type Migrator struct {
	cfg      *config.Config
	gh       *gogithub.Client
	gl       *gogitlab.Client
	logger   hclog.Logger
	ghClient clients.GitHubClient
	glClient clients.GitLabClient
}

// NewMigrator creates a fully initialised Migrator.
func NewMigrator(
	cfg *config.Config,
	gh *gogithub.Client,
	gl *gogitlab.Client,
	ghClient clients.GitHubClient,
	glClient clients.GitLabClient,
	logger hclog.Logger,
) *Migrator {
	return &Migrator{
		cfg:      cfg,
		gh:       gh,
		gl:       gl,
		ghClient: ghClient,
		glClient: glClient,
		logger:   logger,
	}
}

// PerformMigration migrates all projects and writes reports.
func (m *Migrator) PerformMigration(ctx context.Context, projects []CSVRow, collector *ResultCollector, sessionID string) error {
	concurrency := m.cfg.MaxConcurrency
	if len(projects) < concurrency {
		concurrency = len(projects)
	}

	m.logger.Info("processing project(s)", "count", len(projects), "workers", concurrency)

	var wg sync.WaitGroup
	queue := make(chan CSVRow, concurrency*2)
	resultChan := make(chan ProjectResult, concurrency*2)

	collectorDone := make(chan bool)
	go func() {
		for result := range resultChan {
			collector.AddProjectResult(result)
		}
		close(collectorDone)
	}()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for slugs := range queue {
				if err := ctx.Err(); err != nil {
					break
				}
				proj, err := m.newProject(slugs)
				if err != nil {
					m.logger.Error("initializing project", "project", slugs[0], "error", err)
					gitlabPath, githubPath, parseErr := ParseProjectSlugs(slugs)
					if parseErr != nil {
						gitlabPath = []string{"unknown", "unknown"}
						githubPath = []string{"unknown", "unknown"}
					}
					resultChan <- ProjectResult{
						GitLabGroup:   gitlabPath[0],
						GitLabProject: gitlabPath[1],
						GitHubOwner:   githubPath[0],
						GitHubRepo:    githubPath[1],
						Status:        StatusFailed,
						Error:         err.Error(),
						StartTime:     time.Now(),
						EndTime:       time.Now(),
					}
					continue
				}

				result, err := proj.migrate(ctx)
				if err != nil {
					proj.log.Error("migrating project", "error", err)
					result.Status = StatusFailed
					result.Error = err.Error()
				}

				resultChan <- result
			}
		}()
	}

	queueProjects := func() {
		for _, proj := range projects {
			if err := ctx.Err(); err != nil {
				break
			}
			queue <- proj
		}
	}

	if m.cfg.Loop {
		m.logger.Info("looping migration until canceled")
		for {
			if err := ctx.Err(); err != nil {
				break
			}
			queueProjects()
		}
	} else {
		queueProjects()
		close(queue)
	}

	wg.Wait()
	close(resultChan)
	<-collectorDone

	finalReport := collector.Finalize()

	if m.cfg.DetailedReport {
		m.writeDetailedReport(finalReport, sessionID)
	}

	PrintSummaryToConsole(finalReport)

	if finalReport.FailedProjects > 0 || finalReport.PartialProjects > 0 {
		return &MigrationPartialError{
			FailedProjects:  finalReport.FailedProjects,
			PartialProjects: finalReport.PartialProjects,
		}
	}

	return nil
}

func (m *Migrator) writeDetailedReport(finalReport *MigrationReport, sessionID string) {
	exePath, err := os.Executable()
	if err != nil {
		m.logger.Error("failed to get executable path for reports", "error", err)
		return
	}

	reportsDir := filepath.Join(filepath.Dir(exePath), "reports")
	if err := os.MkdirAll(reportsDir, 0755); err != nil {
		m.logger.Error("failed to create reports directory", "error", err)
		return
	}

	jsonPath := filepath.Join(reportsDir, sessionID+"-migration-report.json")
	mdPath := filepath.Join(reportsDir, sessionID+"-migration-report.md")

	m.logger.Info("writing detailed migration reports", "directory", reportsDir, "session", sessionID)

	if err := WriteJSONReport(finalReport, jsonPath); err != nil {
		m.logger.Error("failed to write JSON report", "error", err, "path", jsonPath)
	} else {
		m.logger.Info("JSON report written", "path", jsonPath)
	}

	if err := WriteMarkdownReport(finalReport, mdPath); err != nil {
		m.logger.Error("failed to write Markdown report", "error", err, "path", mdPath)
	} else {
		m.logger.Info("Markdown report written", "path", mdPath)
	}
}

// PrintReport logs a human-readable report for the given projects.
func (m *Migrator) PrintReport(ctx context.Context, projects []CSVRow) {
	m.logger.Debug("building report")

	results := make([]Report, 0)

	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			return
		}

		result, err := m.reportProject(ctx, proj)
		if err != nil {
			m.logger.Error("reporting project", "error", err)
		}

		if result != nil {
			results = append(results, *result)
		}
	}

	fmt.Println()

	totalMergeRequests := 0
	for _, result := range results {
		totalMergeRequests += result.MergeRequestsCount
		fmt.Printf("%s/%s: %d merge requests\n", result.GroupName, result.ProjectName, result.MergeRequestsCount)
	}

	fmt.Println()
	fmt.Printf("Total merge requests: %d\n", totalMergeRequests)
	fmt.Println()
}

// Report holds high-level project report data.
type Report struct {
	GroupName          string
	ProjectName        string
	MergeRequestsCount int
}

func (m *Migrator) reportProject(_ context.Context, slugs []string) (*Report, error) {
	gitlabPath, _, err := ParseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %w", err)
	}

	m.logger.Debug("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
	searchTerm := gitlabPath[1]
	projectResult, _, err := m.gl.Projects.ListProjects(&gogitlab.ListProjectsOptions{Search: &searchTerm})
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}

	var proj *gogitlab.Project
	for _, item := range projectResult {
		if item == nil {
			continue
		}
		if item.PathWithNamespace == slugs[0] {
			m.logger.Debug("found GitLab project", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", item.ID)
			proj = item
		}
	}

	if proj == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	var mergeRequests []*gogitlab.MergeRequest

	opts := &gogitlab.ListProjectMergeRequestsOptions{
		OrderBy: Pointer("created_at"),
		Sort:    Pointer("asc"),
	}

	m.logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", proj.ID)
	for {
		result, resp, err := m.gl.MergeRequests.ListProjectMergeRequests(proj.ID, opts)
		if err != nil {
			return nil, fmt.Errorf("retrieving gitlab merge requests: %w", err)
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	mrCount := len(mergeRequests)
	if m.cfg.SkipOpenMergeRequests {
		mrCount = 0
		for _, mr := range mergeRequests {
			if mr != nil && !strings.EqualFold(mr.State, "opened") {
				mrCount++
			}
		}
	}

	return &Report{
		GroupName:          gitlabPath[0],
		ProjectName:        gitlabPath[1],
		MergeRequestsCount: mrCount,
	}, nil
}
