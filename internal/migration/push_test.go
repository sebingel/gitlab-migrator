package migration

import (
	"errors"
	"net"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/sebingel/gitlab-migrator/internal/config"
)

func TestPushErrHint(t *testing.T) {
	const (
		noForceHint  = " (hint: remove -no-force if push is rejected due to conflicts)"
		workflowHint = " (hint: add 'workflow' scope to your GitHub token to push workflow files)"
	)

	// nonFastForward is the error go-git returns when its own check before a
	// push without force finds that the remote branch is not an ancestor.
	nonFastForward := errors.New("non-fast-forward update: refs/heads/main")

	// remoteRejected returns the error go-git returns when the remote rejects
	// the update of ref with reason.
	remoteRejected := func(ref, reason string) error {
		return (&packp.CommandStatus{ReferenceName: plumbing.ReferenceName(ref), Status: reason}).Error()
	}

	// The text matches the existing workflow scope check in pushErrHint.
	workflowScope := remoteRejected("refs/heads/main", "refusing to allow a Personal Access Token to create or update workflow .github/workflows/ci.yml without 'workflow' scope")

	tests := []struct {
		name    string
		noForce bool
		err     error
		want    string
	}{
		{"nil error", true, nil, ""},
		{"non-fast-forward found by go-git", true, nonFastForward, noForceHint},
		{"non-fast-forward rejected by the remote", true, remoteRejected("refs/heads/main", "non-fast-forward"), noForceHint},
		{"rejected by the remote for another reason", true, remoteRejected("refs/heads/main", "protected branch hook declined"), ""},
		{"branch name contains non-fast-forward", true, remoteRejected("refs/heads/fix/non-fast-forward", "protected branch hook declined"), ""},
		{"missing workflow scope", true, workflowScope, workflowHint},
		// go-git returns this for an annotated tag that differs on the remote,
		// but also for other missing objects, so it gets no hint.
		{"object not found", true, plumbing.ErrObjectNotFound, ""},
		{"authentication required", true, transport.ErrAuthenticationRequired, ""},
		{"authorization failed", true, transport.ErrAuthorizationFailed, ""},
		{"network error", true, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, ""},
		{"non-fast-forward with force push", false, nonFastForward, ""},
		{"missing workflow scope with force push", false, workflowScope, workflowHint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &project{m: &Migrator{cfg: &config.Config{NoForce: tt.noForce}}}
			if got := p.pushErrHint(tt.err); got != tt.want {
				t.Errorf("pushErrHint(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
