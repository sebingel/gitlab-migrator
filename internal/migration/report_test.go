package migration

import (
	"testing"
)

func TestResultCollectorSnapshot_DoesNotChangeCollector(t *testing.T) {
	collector := NewResultCollector()
	collector.AddProjectResult(ProjectResult{GitLabProject: "one", Status: StatusSuccess})

	snap := collector.Snapshot()

	collector.AddProjectResult(ProjectResult{GitLabProject: "two", Status: StatusFailed})

	if snap.TotalProjects != 1 || snap.SuccessProjects != 1 || snap.FailedProjects != 0 {
		t.Errorf("snapshot counts = total %d, success %d, failed %d, want 1, 1, 0",
			snap.TotalProjects, snap.SuccessProjects, snap.FailedProjects)
	}
	if len(snap.Projects) != 1 || snap.Projects[0].GitLabProject != "one" {
		t.Errorf("snapshot projects = %+v, want only project one", snap.Projects)
	}
	if snap.EndTime.IsZero() {
		t.Error("snapshot end time is not set")
	}
	if snap.Duration != snap.EndTime.Sub(snap.StartTime) {
		t.Errorf("snapshot duration = %s, want %s", snap.Duration, snap.EndTime.Sub(snap.StartTime))
	}

	final := collector.Finalize()
	if final.TotalProjects != 2 || len(final.Projects) != 2 {
		t.Errorf("final report has %d projects (total %d), want 2", len(final.Projects), final.TotalProjects)
	}
}
