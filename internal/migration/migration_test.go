package migration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestQueueProjects_StopsWhenCanceledWhileTheQueueIsFull cancels the context
// while the queue is full and no worker is left to receive from it. A send
// that does not watch the context blocks forever, and Ctrl+C never reaches
// the cleanup of PerformMigration.
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
	<-time.After(50 * time.Millisecond)
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
