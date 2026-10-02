package migration

import (
	"testing"
)

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
