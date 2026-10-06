package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
)

// archiveServer is a GitHub test server for the archived repository
// owner/repo. It records the archived value of every repository edit and
// calls onEdit (when set) with it. When unarchiveStatus is set, it answers an
// unarchive edit with that status.
type archiveServer struct {
	mu              sync.Mutex
	edits           []bool
	onEdit          func(r *http.Request, archived bool)
	unarchiveStatus int
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
		if !*edit.Archived && s.unarchiveStatus != 0 {
			http.Error(w, `{"message":"unarchive rejected"}`, s.unarchiveStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The client may have gone away when onEdit waited for a cancel.
		if _, err := fmt.Fprintf(w, `{"name":"repo","archived":%t}`, *edit.Archived); err != nil && r.Context().Err() == nil {
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

func TestMigrate_ReArchivesRepoWhenInterruptedDuringUnarchive(t *testing.T) {
	// Ctrl+C can come while the unarchive request is in flight: GitHub
	// applies the unarchive, but the client only sees the canceled context.
	// The repository must be archived again in that case, too.
	p := newGitLabTestProject(t, http.NewServeMux())
	p.m.cfg.UnarchiveArchivedRepos = true
	p.m.cfg.PullRequestsOnly = true
	gh := serveArchivedGitHubRepo(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh.onEdit = func(r *http.Request, archived bool) {
		if !archived {
			cancel()
			<-r.Context().Done()
		}
	}

	if _, err := p.migrate(ctx); err == nil {
		t.Fatal("migrate: want an error for the interrupted unarchive")
	}
	if got := gh.recordedEdits(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("archived edits = %v, want [false true]", got)
	}
}

// cancelAfterPatch is a transport that cancels the migration context after the
// response of a repository edit has arrived.
type cancelAfterPatch struct {
	cancel context.CancelFunc
}

func (c cancelAfterPatch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if r.Method == http.MethodPatch {
		c.cancel()
	}
	return resp, err
}

func TestMigrate_NoReArchiveWhenUnarchiveFailsBeforeInterrupt(t *testing.T) {
	// When GitHub rejects the unarchive, the repository is still archived.
	// A Ctrl+C that comes after the rejection must not start a re-archive.
	p := newGitLabTestProject(t, http.NewServeMux())
	p.m.cfg.UnarchiveArchivedRepos = true
	p.m.cfg.PullRequestsOnly = true
	gh := serveArchivedGitHubRepo(t, p)
	gh.unarchiveStatus = http.StatusForbidden

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := gogithub.NewClient(&http.Client{Transport: cancelAfterPatch{cancel: cancel}})
	client.BaseURL = p.m.gh.BaseURL
	p.m.gh = client

	if _, err := p.migrate(ctx); err == nil {
		t.Fatal("migrate: want an error for the rejected unarchive")
	}
	if got := gh.recordedEdits(); len(got) != 1 || got[0] {
		t.Fatalf("archived edits = %v, want [false]", got)
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

// serveRepoEditError points the GitHub client of p to a server that answers
// every repository edit with status and message.
func serveRepoEditError(t *testing.T, p *project, status int, message string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := fmt.Fprintf(w, `{"message":%q}`, message); err != nil {
			t.Errorf("writing the error response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := gogithub.NewClient(nil)
	baseURL, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parsing GitHub test server URL: %v", err)
	}
	gh.BaseURL = baseURL
	p.m.gh = gh
}

const archivedHint = "the repository is archived; use -unarchive-archived-repos to unarchive it for the migration"

func TestMirrorRepository_ArchivedRepoErrorNamesUnarchiveFlag(t *testing.T) {
	p := newGitLabTestProject(t, http.NewServeMux())
	serveRepoEditError(t, p, http.StatusForbidden, "Repository was archived so is read-only.")

	err := p.mirrorRepository(context.Background(), true, "main")
	if err == nil {
		t.Fatal("mirrorRepository: want an error for the archived repository")
	}
	if !strings.Contains(err.Error(), "updating github repo") || !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q lost the original update error", err)
	}
	if !strings.Contains(err.Error(), archivedHint) {
		t.Errorf("error %q does not contain the hint %q", err, archivedHint)
	}
}

func TestMirrorRepository_OtherForbiddenErrorGetsNoArchivedHint(t *testing.T) {
	p := newGitLabTestProject(t, http.NewServeMux())
	serveRepoEditError(t, p, http.StatusForbidden, "Resource not accessible by personal access token")

	err := p.mirrorRepository(context.Background(), true, "main")
	if err == nil {
		t.Fatal("mirrorRepository: want an error for the forbidden update")
	}
	if strings.Contains(err.Error(), "-unarchive-archived-repos") {
		t.Errorf("error %q names -unarchive-archived-repos for a 403 that is not about an archived repository", err)
	}
}

func TestMirrorRepository_ArchivedErrorWithOtherStatusGetsNoArchivedHint(t *testing.T) {
	p := newGitLabTestProject(t, http.NewServeMux())
	serveRepoEditError(t, p, http.StatusUnprocessableEntity, "Repository was archived so is read-only.")

	err := p.mirrorRepository(context.Background(), true, "main")
	if err == nil {
		t.Fatal("mirrorRepository: want an error")
	}
	if strings.Contains(err.Error(), "-unarchive-archived-repos") {
		t.Errorf("error %q names -unarchive-archived-repos for a status other than 403", err)
	}
}

func TestMirrorRepository_ArchivedErrorGetsNoHintWhenFlagIsSet(t *testing.T) {
	// The flag is set, so the hint to set it would mislead.
	p := newGitLabTestProject(t, http.NewServeMux())
	p.m.cfg.UnarchiveArchivedRepos = true
	serveRepoEditError(t, p, http.StatusForbidden, "Repository was archived so is read-only.")

	err := p.mirrorRepository(context.Background(), true, "main")
	if err == nil {
		t.Fatal("mirrorRepository: want an error")
	}
	if strings.Contains(err.Error(), "-unarchive-archived-repos") {
		t.Errorf("error %q names -unarchive-archived-repos although it is set", err)
	}
}

func TestAddArchivedHint(t *testing.T) {
	archived := &gogithub.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Message:  "Repository was archived so is read-only.",
	}
	wrapped := fmt.Errorf("creating temporary target branch x on GitHub: %w", archived)

	p := newGitLabTestProject(t, http.NewServeMux())
	got := p.addArchivedHint(wrapped)
	if !strings.Contains(got.Error(), archivedHint) || !strings.Contains(got.Error(), "creating temporary target branch") {
		t.Errorf("addArchivedHint = %q, want the original error and the hint", got)
	}
	if !errors.Is(got, archived) {
		t.Errorf("addArchivedHint lost the wrapped error")
	}

	other := errors.New("boom")
	if got := p.addArchivedHint(other); got != other {
		t.Errorf("addArchivedHint changed an error that is not about an archived repository: %q", got)
	}

	p.m.cfg.UnarchiveArchivedRepos = true
	if got := p.addArchivedHint(wrapped); got != wrapped {
		t.Errorf("addArchivedHint changed the error although -unarchive-archived-repos is set: %q", got)
	}
}
