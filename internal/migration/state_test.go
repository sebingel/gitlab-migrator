package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func testLogger() hclog.Logger {
	return hclog.NewNullLogger()
}

func TestLoadOrCreate_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	s, err := LoadOrCreate(path, "group/project", "owner/repo", testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if s.data.Version != stateFileVersion {
		t.Errorf("version = %d, want %d", s.data.Version, stateFileVersion)
	}
	if s.data.GitLabProject != "group/project" {
		t.Errorf("gitlab_project = %q, want %q", s.data.GitLabProject, "group/project")
	}
	if s.data.GitHubRepo != "owner/repo" {
		t.Errorf("github_repo = %q, want %q", s.data.GitHubRepo, "owner/repo")
	}
	if len(s.data.MergeRequests) != 0 {
		t.Errorf("merge_requests should be empty, got %d", len(s.data.MergeRequests))
	}
}

func TestLoadOrCreate_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	sf := &StateFile{
		Version:       stateFileVersion,
		GitLabProject: "group/project",
		GitHubRepo:    "owner/repo",
		MergeRequests: map[string]*MRState{
			"42": {Status: MRStateSuccess, GitHubPRNum: Pointer(100)},
		},
	}
	data, _ := json.Marshal(sf)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadOrCreate(path, "group/project", "owner/repo", testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if len(s.data.MergeRequests) != 1 {
		t.Fatalf("merge_requests count = %d, want 1", len(s.data.MergeRequests))
	}
	st := s.data.MergeRequests["42"]
	if st == nil || st.Status != MRStateSuccess {
		t.Errorf("MR 42 status = %v, want success", st)
	}
	if st.GitHubPRNum == nil || *st.GitHubPRNum != 100 {
		t.Errorf("MR 42 github_pr_number = %v, want 100", st.GitHubPRNum)
	}
}

func TestLoadOrCreate_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	if err := os.WriteFile(path, []byte("{invalid json"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreate(path, "group/project", "owner/repo", testLogger())
	if err == nil {
		t.Fatal("expected error for corrupt file, got nil")
	}
}

func TestLoadOrCreate_WrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	sf := &StateFile{
		Version:       999,
		GitLabProject: "group/project",
		GitHubRepo:    "owner/repo",
		MergeRequests: map[string]*MRState{},
	}
	data, _ := json.Marshal(sf)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreate(path, "group/project", "owner/repo", testLogger())
	if err == nil {
		t.Fatal("expected error for wrong version, got nil")
	}
}

func TestLoadOrCreate_WrongProjectPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	sf := &StateFile{
		Version:       stateFileVersion,
		GitLabProject: "other/project",
		GitHubRepo:    "other/repo",
		MergeRequests: map[string]*MRState{},
	}
	data, _ := json.Marshal(sf)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreate(path, "group/project", "owner/repo", testLogger())
	if err == nil {
		t.Fatal("expected error for wrong project pair, got nil")
	}
}

func TestLoadOrCreate_NilMergeRequests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	// Write JSON without merge_requests field
	raw := `{"version":1,"gitlab_project":"g/p","github_repo":"o/r"}`
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadOrCreate(path, "g/p", "o/r", testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if s.data.MergeRequests == nil {
		t.Fatal("MergeRequests map should be initialized, got nil")
	}
}

func TestShouldSkip(t *testing.T) {
	s := &MigrationState{
		data: &StateFile{
			MergeRequests: map[string]*MRState{
				"1": {Status: MRStateSuccess},
				"2": {Status: MRStateSkipped},
				"3": {Status: MRStateFailed},
				"4": {Status: MRStatePartial},
			},
		},
		logger: testLogger(),
	}

	tests := []struct {
		iid  int
		want bool
	}{
		{1, true},  // success → skip
		{2, true},  // skipped → skip
		{3, false}, // failed → retry
		{4, false}, // partial → retry
		{5, false}, // not found → process
	}

	for _, tt := range tests {
		got := s.ShouldSkip(tt.iid)
		if got != tt.want {
			t.Errorf("ShouldSkip(%d) = %v, want %v", tt.iid, got, tt.want)
		}
	}
}

func TestGetState(t *testing.T) {
	s := &MigrationState{
		data: &StateFile{
			MergeRequests: map[string]*MRState{
				"10": {Status: MRStateSuccess, GitHubPRNum: Pointer(42)},
			},
		},
		logger: testLogger(),
	}

	st := s.GetState(10)
	if st == nil {
		t.Fatal("expected state for MR 10, got nil")
	}
	if st.Status != MRStateSuccess {
		t.Errorf("status = %q, want %q", st.Status, MRStateSuccess)
	}

	if s.GetState(99) != nil {
		t.Error("expected nil for non-existent MR 99")
	}
}

func TestRecordMethods(t *testing.T) {
	s := &MigrationState{
		data: &StateFile{
			MergeRequests: make(map[string]*MRState),
		},
		logger: testLogger(),
	}

	prNum := 42
	s.RecordSuccess(1, &prNum)
	s.RecordFailure(2, "some error")
	s.RecordSkipped(3, "no commits")
	s.RecordPartial(4, &prNum, "1 comment failed")

	if st := s.data.MergeRequests["1"]; st == nil || st.Status != MRStateSuccess {
		t.Errorf("MR 1: expected success, got %v", st)
	}
	if st := s.data.MergeRequests["2"]; st == nil || st.Status != MRStateFailed || st.Error != "some error" {
		t.Errorf("MR 2: expected failed with error, got %v", st)
	}
	if st := s.data.MergeRequests["3"]; st == nil || st.Status != MRStateSkipped || st.SkipReason != "no commits" {
		t.Errorf("MR 3: expected skipped with reason, got %v", st)
	}
	if st := s.data.MergeRequests["4"]; st == nil || st.Status != MRStatePartial || st.Error != "1 comment failed" {
		t.Errorf("MR 4: expected partial with error, got %v", st)
	}

	if !s.dirty {
		t.Error("expected dirty flag to be true after recording")
	}
}

func TestFlushAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	s, err := LoadOrCreate(path, "g/p", "o/r", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	prNum := 55
	s.RecordSuccess(1, &prNum)
	s.RecordSkipped(2, "branch missing")

	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Verify file exists and is valid JSON
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading flushed file: %v", err)
	}

	var sf StateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("parsing flushed file: %v", err)
	}

	if len(sf.MergeRequests) != 2 {
		t.Fatalf("merge_requests count = %d, want 2", len(sf.MergeRequests))
	}

	// Reload and verify
	s2, err := LoadOrCreate(path, "g/p", "o/r", testLogger())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !s2.ShouldSkip(1) {
		t.Error("MR 1 should be skipped after reload")
	}
	if !s2.ShouldSkip(2) {
		t.Error("MR 2 should be skipped after reload")
	}
}

func TestFlush_NoDirtyNoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	s, _ := LoadOrCreate(path, "g/p", "o/r", testLogger())

	// Flush without any changes — file should not be created
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected no file to be written when not dirty")
	}
}

func TestFlush_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	s, _ := LoadOrCreate(path, "g/p", "o/r", testLogger())

	// First write
	s.RecordFailure(1, "timeout")
	if err := s.Flush(); err != nil {
		t.Fatalf("first Flush: %v", err)
	}

	// Overwrite with success
	s.RecordSuccess(1, Pointer(99))
	if err := s.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}

	// Reload and verify overwrite
	s2, _ := LoadOrCreate(path, "g/p", "o/r", testLogger())
	st := s2.GetState(1)
	if st == nil || st.Status != MRStateSuccess {
		t.Errorf("after overwrite: expected success, got %v", st)
	}
	if st.GitHubPRNum == nil || *st.GitHubPRNum != 99 {
		t.Errorf("after overwrite: expected PR 99, got %v", st.GitHubPRNum)
	}
}

func TestSummary(t *testing.T) {
	s := &MigrationState{
		data: &StateFile{
			MergeRequests: map[string]*MRState{
				"1": {Status: MRStateSuccess},
				"2": {Status: MRStateSuccess},
				"3": {Status: MRStateFailed},
				"4": {Status: MRStateSkipped},
				"5": {Status: MRStatePartial},
				"6": {Status: MRStateSkipped},
			},
		},
		logger: testLogger(),
	}

	total, success, failed, skipped, partial := s.Summary()
	if total != 6 {
		t.Errorf("total = %d, want 6", total)
	}
	if success != 2 {
		t.Errorf("success = %d, want 2", success)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
	if partial != 1 {
		t.Errorf("partial = %d, want 1", partial)
	}
}

func TestSanitizeStateFileName(t *testing.T) {
	tests := []struct {
		glGroup, glProject, ghOwner, ghRepo string
		want                                string
	}{
		{"group", "project", "owner", "repo", "group_project_to_owner_repo"},
		{"my/group", "my/project", "my/owner", "my/repo", "my_group_my_project_to_my_owner_my_repo"},
		{"a b", "c:d", "e\\f", "g", "a_b_c_d_to_e_f_g"},
	}

	for _, tt := range tests {
		got := sanitizeStateFileName(tt.glGroup, tt.glProject, tt.ghOwner, tt.ghRepo)
		if got != tt.want {
			t.Errorf("sanitizeStateFileName(%q, %q, %q, %q) = %q, want %q",
				tt.glGroup, tt.glProject, tt.ghOwner, tt.ghRepo, got, tt.want)
		}
	}
}

func TestFlush_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	s, _ := LoadOrCreate(path, "g/p", "o/r", testLogger())
	s.RecordSuccess(1, Pointer(1))

	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "test.json" {
			t.Errorf("unexpected file left behind: %s", e.Name())
		}
	}
}
