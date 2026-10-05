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
	"github.com/sebingel/gitlab-migrator/internal/clients"
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
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
	queue := make(chan queuedProject, concurrency*2)
	resultChan := make(chan passResult, concurrency*2)

	// With -loop the final report is only written after the loop ends, so the
	// detailed report is also written each time a pass is complete.
	var onPassDone func(pass int)
	if m.cfg.Loop && m.cfg.DetailedReport {
		onPassDone = func(pass int) {
			m.logger.Info("loop pass finished", "pass", pass)
			m.writeDetailedReport(collector.Snapshot(), sessionID)
		}
	}

	collectorDone := make(chan bool)
	go func() {
		collectResults(resultChan, collector, len(projects), onPassDone)
		close(collectorDone)
	}()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				if err := ctx.Err(); err != nil {
					break
				}
				slugs := item.slugs
				proj, err := m.newProject(ctx, slugs)
				if err != nil {
					m.logger.Error("initializing project", "project", slugs[0], "error", err)
					gitlabPath, githubPath, parseErr := ParseProjectSlugs(slugs)
					if parseErr != nil {
						gitlabPath = []string{"unknown", "unknown"}
						githubPath = []string{"unknown", "unknown"}
					}
					resultChan <- passResult{pass: item.pass, result: ProjectResult{
						GitLabGroup:   gitlabPath[0],
						GitLabProject: gitlabPath[1],
						GitHubOwner:   githubPath[0],
						GitHubRepo:    githubPath[1],
						Status:        StatusFailed,
						Error:         err.Error(),
						StartTime:     time.Now(),
						EndTime:       time.Now(),
					}}
					continue
				}

				result, err := proj.migrate(ctx)
				if err != nil {
					proj.log.Error("migrating project", "error", err)
					result.Status = StatusFailed
					result.Error = err.Error()
				}

				resultChan <- passResult{pass: item.pass, result: result}
			}
		}()
	}

	queueProjects := func(pass int) {
		for _, proj := range projects {
			if err := ctx.Err(); err != nil {
				break
			}
			queue <- queuedProject{slugs: proj, pass: pass}
		}
	}

	if m.cfg.Loop {
		m.logger.Info("looping migration until canceled")
		for pass := 1; ; pass++ {
			if err := ctx.Err(); err != nil {
				break
			}
			queueProjects(pass)
		}
	} else {
		queueProjects(1)
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

// queuedProject is one project of one pass over the project list. Without
// -loop there is only pass 1.
type queuedProject struct {
	slugs CSVRow
	pass  int
}

// passResult is the result of a queuedProject.
type passResult struct {
	result ProjectResult
	pass   int
}

// collectResults adds every result to the collector until results is closed.
// When onPassDone is not nil, it is called after the last of the passSize
// results of a pass was added. A pass that was canceled before all of its
// projects ran never completes.
func collectResults(results <-chan passResult, collector *ResultCollector, passSize int, onPassDone func(pass int)) {
	pending := make(map[int]int)
	for r := range results {
		collector.AddProjectResult(r.result)
		if onPassDone == nil {
			continue
		}
		pending[r.pass]++
		if pending[r.pass] == passSize {
			delete(pending, r.pass)
			onPassDone(r.pass)
		}
	}
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

func (m *Migrator) reportProject(ctx context.Context, slugs []string) (*Report, error) {
	gitlabPath, _, err := ParseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %w", err)
	}

	// The project is looked up by its path, like newProject does. A search
	// would need to read all of its pages to find the project.
	m.logger.Debug("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
	proj, _, err := m.gl.Projects.GetProject(slugs[0], nil, gogitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("retrieving project: %w", err)
	}

	if proj == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}
	m.logger.Debug("found GitLab project", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", proj.ID)

	var mergeRequests []*gogitlab.BasicMergeRequest

	opts := mergeRequestListOptions(m.cfg.MergeRequestsAge)

	m.logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", proj.ID)
	for {
		result, resp, err := m.gl.MergeRequests.ListProjectMergeRequests(proj.ID, opts, gogitlab.WithContext(ctx))
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
