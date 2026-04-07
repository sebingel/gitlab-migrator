package migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// MRStateStatus represents the migration status of a single merge request in the state file.
type MRStateStatus string

const (
	MRStateSuccess MRStateStatus = "success"
	MRStateFailed  MRStateStatus = "failed"
	MRStateSkipped MRStateStatus = "skipped"
	MRStatePartial MRStateStatus = "partial"

	stateFileVersion = 1
)

// MRState holds the persisted migration state for a single merge request.
type MRState struct {
	Status      MRStateStatus `json:"status"`
	GitHubPRNum *int          `json:"github_pr_number,omitempty"`
	Error       string        `json:"error,omitempty"`
	SkipReason  string        `json:"skip_reason,omitempty"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// StateFile is the top-level structure serialized to/from the state JSON file.
type StateFile struct {
	Version       int                 `json:"version"`
	GitLabProject string              `json:"gitlab_project"`
	GitHubRepo    string              `json:"github_repo"`
	UpdatedAt     time.Time           `json:"updated_at"`
	MergeRequests map[string]*MRState `json:"merge_requests"`
}

// MigrationState manages the on-disk migration state for a single project pair.
// It is safe for concurrent use from multiple goroutines.
type MigrationState struct {
	mu       sync.Mutex
	filePath string
	data     *StateFile
	dirty    bool
	logger   hclog.Logger
}

// LoadOrCreate loads an existing state file or creates a new empty one.
// Returns an error if the file exists but is corrupt, has an incompatible version,
// or belongs to a different project pair.
func LoadOrCreate(filePath, gitlabProject, githubRepo string, logger hclog.Logger) (*MigrationState, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &MigrationState{
				filePath: filePath,
				data: &StateFile{
					Version:       stateFileVersion,
					GitLabProject: gitlabProject,
					GitHubRepo:    githubRepo,
					MergeRequests: make(map[string]*MRState),
				},
				logger: logger,
			}, nil
		}
		return nil, fmt.Errorf("reading state file %s: %w", filePath, err)
	}

	var sf StateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parsing state file %s (delete to reset): %w", filePath, err)
	}

	if sf.Version != stateFileVersion {
		return nil, fmt.Errorf("unsupported state file version %d in %s (expected %d)", sf.Version, filePath, stateFileVersion)
	}

	if sf.GitLabProject != gitlabProject || sf.GitHubRepo != githubRepo {
		return nil, fmt.Errorf("state file %s belongs to %s -> %s, not %s -> %s",
			filePath, sf.GitLabProject, sf.GitHubRepo, gitlabProject, githubRepo)
	}

	if sf.MergeRequests == nil {
		sf.MergeRequests = make(map[string]*MRState)
	}

	return &MigrationState{
		filePath: filePath,
		data:     &sf,
		logger:   logger,
	}, nil
}

// ShouldSkip returns true if the MR with the given IID was previously completed
// successfully or skipped, and does not need reprocessing.
func (s *MigrationState) ShouldSkip(mrIID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.data.MergeRequests[strconv.Itoa(mrIID)]
	if !ok {
		return false
	}
	return st.Status == MRStateSuccess || st.Status == MRStateSkipped
}

// GetState returns a copy of the stored state for a specific MR IID, or nil if not found.
func (s *MigrationState) GetState(mrIID int) *MRState {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.data.MergeRequests[strconv.Itoa(mrIID)]
	if st == nil {
		return nil
	}
	cp := *st
	return &cp
}

// RecordSuccess records a successful MR migration.
func (s *MigrationState) RecordSuccess(mrIID int, githubPRNum *int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.MergeRequests[strconv.Itoa(mrIID)] = &MRState{
		Status:      MRStateSuccess,
		GitHubPRNum: githubPRNum,
		UpdatedAt:   time.Now(),
	}
	s.dirty = true
}

// RecordFailure records a failed MR migration.
func (s *MigrationState) RecordFailure(mrIID int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.MergeRequests[strconv.Itoa(mrIID)] = &MRState{
		Status:    MRStateFailed,
		Error:     errMsg,
		UpdatedAt: time.Now(),
	}
	s.dirty = true
}

// RecordSkipped records a skipped MR migration.
func (s *MigrationState) RecordSkipped(mrIID int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.MergeRequests[strconv.Itoa(mrIID)] = &MRState{
		Status:     MRStateSkipped,
		SkipReason: reason,
		UpdatedAt:  time.Now(),
	}
	s.dirty = true
}

// RecordPartial records a partially successful MR migration (PR created but some comments failed).
func (s *MigrationState) RecordPartial(mrIID int, githubPRNum *int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.MergeRequests[strconv.Itoa(mrIID)] = &MRState{
		Status:      MRStatePartial,
		GitHubPRNum: githubPRNum,
		Error:       errMsg,
		UpdatedAt:   time.Now(),
	}
	s.dirty = true
}

// Flush writes the current state to disk atomically (write to temp file, fsync, then rename).
// On Windows, os.Rename cannot overwrite an existing file, so we remove the old file first.
// The worst case on crash during the microsecond window between remove and rename is losing
// the state file entirely, which falls back to full API-based re-check (the status quo).
func (s *MigrationState) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dirty {
		return nil
	}

	s.data.UpdatedAt = time.Now()

	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.filePath)
	tmpFile, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp state file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp state file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp state file: %w", err)
	}

	if runtime.GOOS == "windows" {
		_ = os.Remove(s.filePath)
	}

	if err := os.Rename(tmpPath, s.filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp state file: %w", err)
	}

	s.dirty = false
	return nil
}

// Summary returns counts of each status for logging purposes.
func (s *MigrationState) Summary() (total, success, failed, skipped, partial int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, st := range s.data.MergeRequests {
		total++
		switch st.Status {
		case MRStateSuccess:
			success++
		case MRStateFailed:
			failed++
		case MRStateSkipped:
			skipped++
		case MRStatePartial:
			partial++
		}
	}
	return
}

// sanitizeStateFileName builds a safe filename from project path components.
func sanitizeStateFileName(gitlabGroup, gitlabProject, githubOwner, githubRepo string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return r.Replace(gitlabGroup) + "_" + r.Replace(gitlabProject) +
		"_to_" + r.Replace(githubOwner) + "_" + r.Replace(githubRepo)
}
