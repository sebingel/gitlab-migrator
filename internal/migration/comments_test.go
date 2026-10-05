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
	"github.com/sebingel/gitlab-migrator/internal/config"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// fakeGitLabUsers is a GitLabClient that knows every user without a GitHub
// profile, so the author of a comment is shown by name.
type fakeGitLabUsers struct{}

func (fakeGitLabUsers) GetUser(context.Context, string) (*gogitlab.User, error) {
	return &gogitlab.User{}, nil
}

// commentServer is a GitHub test server for the comments of pull request 7. It
// records the created comments and the edited comment IDs.
type commentServer struct {
	mu      sync.Mutex
	created []string
	edited  []int64

	// failEdit makes every edit of a comment answer with an error.
	failEdit bool
}

func newCommentProject(t *testing.T, cs *commentServer) *project {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/owner/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		var c gogithub.IssueComment
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("decoding the created comment: %v", err)
		}
		cs.mu.Lock()
		cs.created = append(cs.created, c.GetBody())
		id := int64(1000 + len(cs.created))
		cs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if _, err := fmt.Fprintf(w, `{"id":%d}`, id); err != nil {
			t.Errorf("writing the created comment: %v", err)
		}
	})
	mux.HandleFunc("PATCH /repos/owner/repo/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		if _, err := fmt.Sscan(r.PathValue("id"), &id); err != nil {
			t.Errorf("parsing the comment ID: %v", err)
		}
		cs.mu.Lock()
		cs.edited = append(cs.edited, id)
		cs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if cs.failEdit {
			w.WriteHeader(http.StatusInternalServerError)
			if _, err := fmt.Fprint(w, `{"message":"edit failed in test"}`); err != nil {
				t.Errorf("writing the error: %v", err)
			}
			return
		}
		if _, err := fmt.Fprintf(w, `{"id":%d}`, id); err != nil {
			t.Errorf("writing the edited comment: %v", err)
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

	return &project{
		m: &Migrator{
			cfg:      &config.Config{},
			gh:       gh,
			glClient: fakeGitLabUsers{},
			logger:   hclog.NewNullLogger(),
		},
		log:        hclog.NewNullLogger(),
		gitlabPath: []string{"group", "project"},
		githubPath: []string{"owner", "repo"},
	}
}

func gitLabNote(id int64, body string) *gogitlab.Note {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &gogitlab.Note{
		ID:        id,
		Body:      body,
		Author:    gogitlab.NoteAuthor{Username: "someone", Name: "Some One"},
		CreatedAt: &created,
	}
}

// migratedComment is an existing GitHub comment that the tool created for the
// GitLab note noteID earlier, with an outdated text.
func migratedComment(commentID, noteID int64) *gogithub.IssueComment {
	body := fmt.Sprintf("> | **Original Author** | Some One |\n> | **Note ID** | %d |\n> | **Date Originally Created** | 2026-01-02 |\n\n## Original Comment\n\nold text", noteID)
	return &gogithub.IssueComment{ID: &commentID, Body: &body}
}

func TestMigrateComments_NoteIDMatchesTheWholeTableCell(t *testing.T) {
	cs := &commentServer{}
	p := newCommentProject(t, cs)
	var result MergeRequestResult

	// The comment of note 123 must not be taken for the comment of note 12.
	p.migrateComments(context.Background(), &gogithub.PullRequest{Number: Pointer(7)},
		[]*gogitlab.Note{gitLabNote(12, "text of note 12")},
		[]*gogithub.IssueComment{migratedComment(500, 123)}, &result)

	if len(cs.edited) != 0 {
		t.Errorf("edited comments = %v, want none: the comment of note 123 is not the comment of note 12", cs.edited)
	}
	if len(cs.created) != 1 {
		t.Errorf("created comments = %d, want 1 for note 12", len(cs.created))
	}
	if len(result.Comments) != 1 || result.Comments[0].GitLabNoteID != 12 || result.Comments[0].Status != StatusSuccess {
		t.Errorf("comment results = %+v, want one success for note 12", result.Comments)
	}
	if result.MigratedComments != 1 || result.FailedComments != 0 {
		t.Errorf("migrated/failed = %d/%d, want 1/0", result.MigratedComments, result.FailedComments)
	}
}

func TestMigrateComments_FailedUpdateIsReportedOnce(t *testing.T) {
	cs := &commentServer{failEdit: true}
	p := newCommentProject(t, cs)
	var result MergeRequestResult

	p.migrateComments(context.Background(), &gogithub.PullRequest{Number: Pointer(7)},
		[]*gogitlab.Note{gitLabNote(12, "new text")},
		[]*gogithub.IssueComment{migratedComment(500, 12)}, &result)

	if len(result.Comments) != 1 {
		t.Fatalf("comment results = %+v, want one entry for the failed update", result.Comments)
	}
	if result.Comments[0].Status != StatusFailed {
		t.Errorf("status = %q, want %q", result.Comments[0].Status, StatusFailed)
	}
	if result.FailedComments != 1 || result.MigratedComments != 0 {
		t.Errorf("failed/migrated = %d/%d, want 1/0", result.FailedComments, result.MigratedComments)
	}
	if len(cs.created) != 0 {
		t.Errorf("created comments = %d, want none: the comment exists", len(cs.created))
	}
}

func TestMigrateComments_UpdatesOnlyTheFirstMatchingComment(t *testing.T) {
	cs := &commentServer{}
	p := newCommentProject(t, cs)
	var result MergeRequestResult

	p.migrateComments(context.Background(), &gogithub.PullRequest{Number: Pointer(7)},
		[]*gogitlab.Note{gitLabNote(12, "new text")},
		[]*gogithub.IssueComment{migratedComment(500, 12), migratedComment(501, 12)}, &result)

	if len(cs.edited) != 1 || cs.edited[0] != 500 {
		t.Errorf("edited comments = %v, want only [500]", cs.edited)
	}
	if len(result.Comments) != 1 || result.Comments[0].GitHubCommentID == nil || *result.Comments[0].GitHubCommentID != 500 {
		t.Errorf("comment results = %+v, want one entry for comment 500", result.Comments)
	}
	if result.MigratedComments != 1 || result.FailedComments != 0 {
		t.Errorf("migrated/failed = %d/%d, want 1/0", result.MigratedComments, result.FailedComments)
	}
}

func TestMigrateComments_NoteIDInTheOriginalTextIsNotAMatch(t *testing.T) {
	cs := &commentServer{}
	p := newCommentProject(t, cs)
	var result MergeRequestResult

	// The comment of note 99 quotes the header line of note 12 in its text. It
	// must not be taken for the comment of note 12.
	quoting := migratedComment(500, 99)
	body := *quoting.Body + "\n\n> | **Note ID** | 12 |\n"
	quoting.Body = &body

	p.migrateComments(context.Background(), &gogithub.PullRequest{Number: Pointer(7)},
		[]*gogitlab.Note{gitLabNote(12, "text of note 12")},
		[]*gogithub.IssueComment{quoting}, &result)

	if len(cs.edited) != 0 {
		t.Errorf("edited comments = %v, want none: the quoted header line is part of the text of note 99", cs.edited)
	}
	if len(cs.created) != 1 {
		t.Errorf("created comments = %d, want 1 for note 12", len(cs.created))
	}
}

func TestMigrateComments_CommentWithoutOriginalCommentHeadingIsNotAMatch(t *testing.T) {
	cs := &commentServer{}
	p := newCommentProject(t, cs)
	var result MergeRequestResult

	// A comment that a person wrote on the pull request has no generated
	// header, so a quoted header line in it must not make it a match.
	commentID := int64(500)
	body := "I saw this line in another comment:\n> | **Note ID** | 12 |\n"
	personal := &gogithub.IssueComment{ID: &commentID, Body: &body}

	p.migrateComments(context.Background(), &gogithub.PullRequest{Number: Pointer(7)},
		[]*gogitlab.Note{gitLabNote(12, "text of note 12")},
		[]*gogithub.IssueComment{personal}, &result)

	if len(cs.edited) != 0 {
		t.Errorf("edited comments = %v, want none: the comment is not a migrated comment", cs.edited)
	}
	if len(cs.created) != 1 {
		t.Errorf("created comments = %d, want 1 for note 12", len(cs.created))
	}
}
