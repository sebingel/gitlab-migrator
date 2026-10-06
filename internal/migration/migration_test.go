package migration

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// TestPerformMigration_LoopStartsNextPassAfterPreviousPass runs -loop with a
// slow and a fast project. A new pass that does not wait for the previous one
// lets the worker of the fast project start the slow project again while the
// other worker still runs it.
func TestPerformMigration_LoopStartsNextPassAfterPreviousPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const stopAfter = 6 // three passes over two projects
	var (
		mu       sync.Mutex
		running  = make(map[string]int)
		overlaps []string
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		mu.Lock()
		running[path]++
		if running[path] > 1 {
			overlaps = append(overlaps, path)
		}
		requests++
		if requests == stopAfter {
			cancel()
		}
		mu.Unlock()

		if strings.HasSuffix(path, "slow") {
			time.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}

		mu.Lock()
		running[path]--
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	gl, err := gogitlab.NewClient("test-token", gogitlab.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("creating GitLab client: %v", err)
	}
	m := &Migrator{
		cfg:    &config.Config{Loop: true, MaxConcurrency: 4},
		gl:     gl,
		logger: hclog.NewNullLogger(),
	}
	projects := []CSVRow{{"group/slow", "owner/slow"}, {"group/fast", "owner/fast"}}

	done := make(chan struct{})
	go func() {
		_ = m.PerformMigration(ctx, projects, NewResultCollector(), "test-session")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PerformMigration did not return after the context was canceled")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(overlaps) > 0 {
		t.Errorf("projects ran twice at the same time: %v", overlaps)
	}
	if requests < stopAfter {
		t.Errorf("got %d project lookups, want at least %d", requests, stopAfter)
	}
}

// TestPerformMigration_LoopReturnsWhenCanceledWithIdleWorkers cancels -loop
// while one worker waits in the queue for the next project and the other one
// still runs the slow project of the pass. The idle worker only stops when the
// queue is closed, so PerformMigration must close it after the loop.
func TestPerformMigration_LoopReturnsWhenCanceledWithIdleWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fastDone := make(chan struct{})
	slowStarted := make(chan struct{})
	release := make(chan struct{})
	var fastOnce, slowOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "slow") {
			slowOnce.Do(func() { close(slowStarted) })
			select {
			case <-r.Context().Done():
			case <-release:
			}
		} else {
			defer fastOnce.Do(func() { close(fastDone) })
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	defer close(release)

	gl, err := gogitlab.NewClient("test-token", gogitlab.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("creating GitLab client: %v", err)
	}
	m := &Migrator{
		cfg:    &config.Config{Loop: true, MaxConcurrency: 4},
		gl:     gl,
		logger: hclog.NewNullLogger(),
	}
	projects := []CSVRow{{"group/slow", "owner/slow"}, {"group/fast", "owner/fast"}}

	done := make(chan struct{})
	go func() {
		_ = m.PerformMigration(ctx, projects, NewResultCollector(), "test-session")
		close(done)
	}()

	for _, ch := range []chan struct{}{slowStarted, fastDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("the projects of the first pass did not start")
		}
	}
	// Give the worker of the fast project time to go back to the queue.
	<-time.After(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PerformMigration did not return after the context was canceled")
	}
}

// TestQueueProjects_StopsWhenCanceledWhileTheQueueIsFull cancels the context
// while the queue is full and nothing receives from it. queueProjects must
// return without help from a receiver.
func TestQueueProjects_StopsWhenCanceledWhileTheQueueIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	queue := make(chan queuedProject, 1)
	projects := []CSVRow{{"group/a", "owner/a"}, {"group/b", "owner/b"}}

	done := make(chan struct{})
	go func() {
		queueProjects(ctx, queue, projects, 1)
		close(done)
	}()

	// The first project fills the queue; the send of the second one blocks.
	// Wait until the queue is full, so that cancel cannot come before the
	// first send and end queueProjects at its ctx.Err check instead.
	deadline := time.Now().Add(5 * time.Second)
	for len(queue) < cap(queue) {
		if time.Now().After(deadline) {
			t.Fatal("queueProjects did not fill the queue")
		}
		<-time.After(time.Millisecond)
	}
	// Give the goroutine time to reach the blocked send of the second project.
	<-time.After(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queueProjects did not return after the context was canceled")
	}
}

func TestCollectResults_CallsOnPassDoneWhenAPassIsComplete(t *testing.T) {
	collector := NewResultCollector()
	results := make(chan passResult)

	type call struct {
		pass      int
		collected int
	}
	var calls []call
	done := make(chan struct{})
	go func() {
		collectResults(results, collector, 2, func(pass int) {
			calls = append(calls, call{pass: pass, collected: collector.Snapshot().TotalProjects})
		})
		close(done)
	}()

	// A slow project of pass 1 finishes after a project of pass 2.
	results <- passResult{pass: 1, result: ProjectResult{GitLabProject: "a"}}
	results <- passResult{pass: 2, result: ProjectResult{GitLabProject: "a"}}
	results <- passResult{pass: 1, result: ProjectResult{GitLabProject: "b"}}
	results <- passResult{pass: 2, result: ProjectResult{GitLabProject: "b"}}
	// Pass 3 is canceled after one project, so it never completes.
	results <- passResult{pass: 3, result: ProjectResult{GitLabProject: "a"}}
	close(results)
	<-done

	want := []call{{pass: 1, collected: 3}, {pass: 2, collected: 4}}
	if len(calls) != len(want) {
		t.Fatalf("onPassDone calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("onPassDone call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
	if got := collector.Finalize().TotalProjects; got != 5 {
		t.Errorf("collected %d results, want 5", got)
	}
}

func TestCollectResults_WithoutOnPassDoneAddsAllResults(t *testing.T) {
	collector := NewResultCollector()
	results := make(chan passResult, 3)
	results <- passResult{pass: 1, result: ProjectResult{GitLabProject: "a", Status: StatusSuccess}}
	results <- passResult{pass: 1, result: ProjectResult{GitLabProject: "b", Status: StatusFailed}}
	results <- passResult{pass: 1, result: ProjectResult{GitLabProject: "c", Status: StatusPartial}}
	close(results)

	collectResults(results, collector, 3, nil)

	report := collector.Finalize()
	if report.TotalProjects != 3 || report.SuccessProjects != 1 || report.FailedProjects != 1 || report.PartialProjects != 1 {
		t.Errorf("report counts = total %d, success %d, failed %d, partial %d, want 3, 1, 1, 1",
			report.TotalProjects, report.SuccessProjects, report.FailedProjects, report.PartialProjects)
	}
}

// TestWriteReport_ListsFailedProjectsAndReturnsError reports a project that
// GitLab has, one that GitLab does not have, and one with an invalid GitHub
// slug. The two failed projects must not count as projects with 0 merge
// requests: they are listed with their error, and the report returns an error,
// so that main exits with code 1 (issue #140).
func TestWriteReport_ListsFailedProjectsAndReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	serveProjects(t, mux, []*gogitlab.Project{{ID: 1, PathWithNamespace: "group/project"}})
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests", []*gogitlab.BasicMergeRequest{{IID: 1, State: "merged"}, {IID: 2, State: "closed"}}, &calls)
	p := newGitLabTestProject(t, mux)

	projects := []CSVRow{
		{"group/project", "owner/repo"},
		{"does/not/exist", "owner/other"},
		{"group/project", "notaslug"},
	}

	var out bytes.Buffer
	err := p.m.writeReport(context.Background(), &out, projects)
	if err == nil {
		t.Fatal("writeReport returned no error, want one for the 2 failed projects")
	}
	if !strings.Contains(err.Error(), "report 2 of 3") {
		t.Errorf("error = %q, want it to name 2 of 3 projects", err)
	}

	got := out.String()
	for _, want := range []string{
		"group/project: 2 merge requests",
		"does/not/exist,owner/other: retrieving project:",
		"group/project,notaslug: parsing project slugs: invalid GitHub project: notaslug",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "does/not/exist: 0 merge requests") {
		t.Errorf("output counts the failed project with 0 merge requests:\n%s", got)
	}
}

func TestWriteReport_ReturnsNoErrorWhenAllProjectsAreReported(t *testing.T) {
	mux := http.NewServeMux()
	serveProjects(t, mux, []*gogitlab.Project{{ID: 1, PathWithNamespace: "group/project"}})
	var calls atomic.Int32
	servePages(t, mux, "/api/v4/projects/1/merge_requests", []*gogitlab.BasicMergeRequest{{IID: 1, State: "merged"}}, &calls)
	p := newGitLabTestProject(t, mux)

	var out bytes.Buffer
	if err := p.m.writeReport(context.Background(), &out, []CSVRow{{"group/project", "owner/repo"}}); err != nil {
		t.Fatalf("writeReport: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Total merge requests: 1\n") {
		t.Errorf("output does not contain the total of 1:\n%s", got)
	}
	if strings.Contains(got, "could not be reported") {
		t.Errorf("output lists failed projects, want none:\n%s", got)
	}
}

// TestWriteReport_StopsWhenCanceledAndCountsTheStoppedProject cancels the
// report while the first of two projects runs. Like PerformMigration, the
// report starts no further project, and the project that the cancel stopped
// counts as failed, so the report still returns an error.
func TestWriteReport_StopsWhenCanceledAndCountsTheStoppedProject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		cancel()
		<-r.Context().Done()
	})
	p := newGitLabTestProject(t, mux)

	projects := []CSVRow{
		{"group/first", "owner/first"},
		{"group/second", "owner/second"},
	}

	var out bytes.Buffer
	err := p.m.writeReport(ctx, &out, projects)
	if err == nil || !strings.Contains(err.Error(), "report 1 of 2") {
		t.Errorf("error = %v, want one that names 1 of 2 projects", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("GitLab got %d project requests, want 1: the second project must not start after the cancel", n)
	}
	if got := out.String(); !strings.Contains(got, "group/first,owner/first: retrieving project:") {
		t.Errorf("output does not list the stopped project:\n%s", got)
	}
}

// TestWriteReport_CancelBeforeAProjectStartsIsNotAFullReport cancels the
// report before any project starts, so no project fails. The report must
// still say that projects were not started and return an error: else it
// looks like a complete report and the tool exits with code 0.
func TestWriteReport_CancelBeforeAProjectStartsIsNotAFullReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	})
	p := newGitLabTestProject(t, mux)

	projects := []CSVRow{
		{"group/first", "owner/first"},
		{"group/second", "owner/second"},
	}

	var out bytes.Buffer
	err := p.m.writeReport(ctx, &out, projects)
	if err == nil || !strings.Contains(err.Error(), "2 of 2 project(s) not started") {
		t.Errorf("error = %v, want one that names 2 of 2 projects not started", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("GitLab got %d project requests, want 0 after the cancel", n)
	}
	if got := out.String(); !strings.Contains(got, "(without the 2 project(s) not started after the cancel)") {
		t.Errorf("output does not say that projects were not started:\n%s", got)
	}
}
