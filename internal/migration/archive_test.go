package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
)

// archiveServer is a GitHub test server for the archived repository
// owner/repo. It records the archived value of every repository edit and
// calls onEdit (when set) with it.
type archiveServer struct {
	mu     sync.Mutex
	edits  []bool
	onEdit func(r *http.Request, archived bool)
}

func (s *archiveServer) recordedEdits() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.edits...)
}

// serveArchivedGitHubRepo points the GitHub client of p to an archiveServer.
func serveArchivedGitHubRepo(t *testing.T, p *project) *archiveServer {
	t.Helper()
	s := &archiveServer{}
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("GET /repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"name":"repo","archived":true}`); err != nil {
			t.Errorf("writing the GitHub repository: %v", err)
		}
	})
	ghMux.HandleFunc("PATCH /repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		var edit struct {
			Archived *bool `json:"archived"`
		}
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil || edit.Archived == nil {
			t.Errorf("repository edit without archived value: %v", err)
			http.Error(w, "bad edit", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.edits = append(s.edits, *edit.Archived)
		s.mu.Unlock()
		if s.onEdit != nil {
			s.onEdit(r, *edit.Archived)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"name":"repo","archived":%t}`, *edit.Archived); err != nil {
			t.Errorf("writing the edited GitHub repository: %v", err)
		}
	})
	ghSrv := httptest.NewServer(ghMux)
	t.Cleanup(ghSrv.Close)
	gh := gogithub.NewClient(nil)
	baseURL, err := url.Parse(ghSrv.URL + "/")
	if err != nil {
		t.Fatalf("parsing GitHub test server URL: %v", err)
	}
	gh.BaseURL = baseURL
	p.m.gh = gh
	return s
}

func TestMigrate_ReArchivesRepoAfterInterrupt(t *testing.T) {
	// With -unarchive-archived-repos the repository must be archived again
	// even when the run is interrupted (Ctrl+C) after it was unarchived.
	p := newGitLabTestProject(t, http.NewServeMux())
	p.m.cfg.UnarchiveArchivedRepos = true
	p.m.cfg.PullRequestsOnly = true
	gh := serveArchivedGitHubRepo(t, p)

	// The cancel comes with the log line that is written after the repository
	// was unarchived, so no request is in flight.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.log = hclog.New(&hclog.LoggerOptions{
		Level:  hclog.Info,
		Output: cancelOnLog{msg: "pull-requests-only mode: skipping repository clone and push", cancel: cancel},
	})

	if _, err := p.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := gh.recordedEdits(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("archived edits = %v, want [false true]", got)
	}
}

func TestMigrate_ReArchiveAfterInterruptStopsAfterGracePeriod(t *testing.T) {
	// After Ctrl+C the re-archive must not keep the program running for
	// longer than the grace period, for example when GitHub does not answer.
	oldGrace := rearchiveGracePeriod
	rearchiveGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { rearchiveGracePeriod = oldGrace })

	p := newGitLabTestProject(t, http.NewServeMux())
	p.m.cfg.UnarchiveArchivedRepos = true
	p.m.cfg.PullRequestsOnly = true
	gh := serveArchivedGitHubRepo(t, p)
	// Registered after the server, so it runs before the server closes.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	gh.onEdit = func(r *http.Request, archived bool) {
		if archived {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.log = hclog.New(&hclog.LoggerOptions{
		Level:  hclog.Info,
		Output: cancelOnLog{msg: "pull-requests-only mode: skipping repository clone and push", cancel: cancel},
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.migrate(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-archive did not stop after the grace period")
	}
	if got := gh.recordedEdits(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("archived edits = %v, want [false true]", got)
	}
}
