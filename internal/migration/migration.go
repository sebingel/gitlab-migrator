package migration

import (
	"context"
	"fmt"
	"io"
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

	// With -loop the next pass is queued only after every project of the
	// previous pass has finished, so the work of two passes never overlaps.
	// Only one pass runs at a time, so one buffered slot is enough and the
	// collector never blocks on it, even after the loop has stopped.
	// The final report is only written after the loop ends, so the detailed
	// report is also written each time a pass is complete.
	var onPassDone func(pass int)
	passDone := make(chan struct{}, 1)
	if m.cfg.Loop {
		onPassDone = func(pass int) {
			m.logger.Info("loop pass finished", "pass", pass)
			if m.cfg.DetailedReport {
				m.writeDetailedReport(collector.Snapshot(), sessionID)
			}
			passDone <- struct{}{}
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

	if m.cfg.Loop {
		m.logger.Info("looping migration until canceled")
		for pass := 1; ; pass++ {
			if err := ctx.Err(); err != nil {
				break
			}
			queueProjects(ctx, queue, projects, pass)
			select {
			case <-passDone:
			case <-ctx.Done():
			}
		}
	} else {
		queueProjects(ctx, queue, projects, 1)
	}
	// Workers that wait for the next project stop when the queue is closed.
	close(queue)

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

// queueProjects sends every project of the list to the queue as part of the
// given pass. It stops when ctx is canceled, also while it waits for a free
// slot. In PerformMigration a plain send would not hang, because each worker
// takes one more item before it sees the cancel. The send watches ctx anyway,
// so the sender does not depend on that.
func queueProjects(ctx context.Context, queue chan<- queuedProject, projects []CSVRow, pass int) {
	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			return
		}
		select {
		case queue <- queuedProject{slugs: proj, pass: pass}:
		case <-ctx.Done():
			return
		}
	}
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

// PrintReport prints a human-readable report for the given projects to stdout.
// It returns an error when at least one project could not be reported.
func (m *Migrator) PrintReport(ctx context.Context, projects []CSVRow) error {
	return m.writeReport(ctx, os.Stdout, projects)
}

// failedReport is a project that could not be reported.
type failedReport struct {
	project string
	err     error
}

// writeReport writes the report of PrintReport to w. A project that cannot be
// reported is listed with its error, and is not counted with 0 merge requests.
// When ctx is canceled, no further project is started, like in
// PerformMigration: the report then shows the projects done so far, and a
// project that the cancel stopped counts as failed.
func (m *Migrator) writeReport(ctx context.Context, w io.Writer, projects []CSVRow) error {
	m.logger.Debug("building report")

	results := make([]Report, 0, len(projects))
	var failed []failedReport

	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			break
		}

		name := strings.Join(proj, ",")
		result, err := m.reportProject(ctx, proj)
		if err != nil {
			m.logger.Error("reporting project", "project", name, "error", err)
			failed = append(failed, failedReport{project: name, err: err})
			continue
		}

		results = append(results, *result)
	}

	var b strings.Builder
	b.WriteString("\n")

	totalMergeRequests := 0
	for _, result := range results {
		totalMergeRequests += result.MergeRequestsCount
		fmt.Fprintf(&b, "%s/%s: %d merge requests\n", result.GroupName, result.ProjectName, result.MergeRequestsCount)
	}

	b.WriteString("\n")
	if len(failed) == 0 {
		fmt.Fprintf(&b, "Total merge requests: %d\n", totalMergeRequests)
	} else {
		fmt.Fprintf(&b, "Total merge requests: %d (without the %d project(s) that could not be reported)\n", totalMergeRequests, len(failed))
		b.WriteString("\n")
		fmt.Fprintf(&b, "Projects that could not be reported (%d):\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(&b, "  %s: %v\n", f.project, f.err)
		}
	}
	b.WriteString("\n")

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}

	if len(failed) > 0 {
		return fmt.Errorf("could not report %d of %d project(s)", len(failed), len(projects))
	}
	return nil
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
