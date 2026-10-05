package clients

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestGetUser_StopsWhenContextIsCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// Close waits for running requests, so release them first.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	gl, err := gogitlab.NewClient("test-token", gogitlab.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("creating GitLab client: %v", err)
	}
	c := NewGitLabClient(gl, hclog.NewNullLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.GetUser(ctx, "someone")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no GitLab request arrived")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the GitLab request did not stop after the context was canceled")
	}
}
