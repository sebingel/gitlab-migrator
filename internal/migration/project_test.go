package migration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
)

func TestNewProject_RenameMasterToMainWarnsWhenDefaultBranchIsNotMaster(t *testing.T) {
	const warning = `-rename-master-to-main: the default branch of the GitLab project is "develop", not "master"; it will be renamed to "main"`

	tests := []struct {
		name          string
		defaultBranch string
		renameMaster  bool
		renameTrunk   string
		wantWarning   bool
		wantBranch    string
	}{
		{name: "flag and default branch develop", defaultBranch: "develop", renameMaster: true, wantWarning: true, wantBranch: "main"},
		{name: "flag and default branch master", defaultBranch: "master", renameMaster: true, wantWarning: false, wantBranch: "main"},
		{name: "flag and no default branch", defaultBranch: "", renameMaster: true, wantWarning: false, wantBranch: "main"},
		{name: "no flag and default branch develop", defaultBranch: "develop", wantWarning: false, wantBranch: "develop"},
		{name: "trunk branch and default branch develop", defaultBranch: "develop", renameTrunk: "trunk", wantWarning: false, wantBranch: "trunk"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v4/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := fmt.Fprintf(w, `{"id":1,"default_branch":%q}`, tt.defaultBranch); err != nil {
					t.Errorf("writing the GitLab project: %v", err)
				}
			})
			base := newGitLabTestProject(t, mux)

			var logs bytes.Buffer
			base.m.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, Level: hclog.Debug})
			base.m.cfg.RenameMasterToMain = tt.renameMaster
			base.m.cfg.RenameTrunkBranch = tt.renameTrunk

			p, err := base.m.newProject(context.Background(), []string{"group/project", "owner/repo"})
			if err != nil {
				t.Fatalf("newProject: %v", err)
			}
			if p.defaultBranch != tt.wantBranch {
				t.Errorf("defaultBranch = %q, want %q", p.defaultBranch, tt.wantBranch)
			}
			if got := strings.Contains(logs.String(), warning); got != tt.wantWarning {
				t.Errorf("warning logged = %v, want %v; log:\n%s", got, tt.wantWarning, logs.String())
			}
			if tt.wantWarning && !strings.Contains(logs.String(), "[WARN]") {
				t.Errorf("the message is not logged at WARN level; log:\n%s", logs.String())
			}
		})
	}
}

func makeGitHubError(statusCode int, message string, errors []gogithub.Error) error {
	return &gogithub.ErrorResponse{
		Response: &http.Response{StatusCode: statusCode},
		Message:  message,
		Errors:   errors,
	}
}

// A typed nil *ErrorResponse passes errors.As with a nil target. The
// classifiers must return false for it, not panic on ghErr.Response.
func TestGitHubErrorClassifiersTypedNil(t *testing.T) {
	var err error = (*gogithub.ErrorResponse)(nil)
	classifiers := map[string]func(error) bool{
		"isAlreadyExistsError":         isAlreadyExistsError,
		"isAlreadyExistsPRError":       isAlreadyExistsPRError,
		"isSearchSyntaxError":          isSearchSyntaxError,
		"isReferenceUpdateFailedError": isReferenceUpdateFailedError,
		"isGitHubNotFound":             isGitHubNotFound,
	}
	for name, classify := range classifiers {
		t.Run(name, func(t *testing.T) {
			if classify(err) {
				t.Errorf("%s(typed nil) = true, want false", name)
			}
		})
	}
}

func TestIsAlreadyExistsPRError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "non-ErrorResponse error",
			err:  fmt.Errorf("some random error"),
			want: false,
		},
		{
			name: "422 with matching top-level message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "A pull request already exists for org:branch", nil),
			want: true,
		},
		{
			name: "422 with matching nested error message",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "A pull request already exists for org:branch"},
			}),
			want: true,
		},
		{
			name: "422 with unrelated message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Something else went wrong", nil),
			want: false,
		},
		{
			name: "404 with matching message - wrong status code",
			err:  makeGitHubError(http.StatusNotFound, "A pull request already exists for org:branch", nil),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyExistsPRError(tt.err); got != tt.want {
				t.Errorf("isAlreadyExistsPRError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsAlreadyExistsError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "422 with matching top-level message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Reference already exists", nil),
			want: true,
		},
		{
			name: "422 with matching nested error message",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "Reference already exists"},
			}),
			want: true,
		},
		{
			name: "422 with unrelated message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Something else", nil),
			want: false,
		},
		{
			name: "non-ErrorResponse error",
			err:  fmt.Errorf("Reference already exists"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyExistsError(tt.err); got != tt.want {
				t.Errorf("isAlreadyExistsError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsReferenceUpdateFailedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "422 with matching message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Reference update failed", nil),
			want: true,
		},
		{
			name: "422 with unrelated message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Reference already exists", nil),
			want: false,
		},
		{
			name: "404 with matching message - wrong status code",
			err:  makeGitHubError(http.StatusNotFound, "Reference update failed", nil),
			want: false,
		},
		{
			name: "non-ErrorResponse error",
			err:  fmt.Errorf("Reference update failed"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReferenceUpdateFailedError(tt.err); got != tt.want {
				t.Errorf("isReferenceUpdateFailedError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsGitHubNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "404 error",
			err:  makeGitHubError(http.StatusNotFound, "Not Found", nil),
			want: true,
		},
		{
			name: "422 error - not a 404",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Unprocessable Entity", nil),
			want: false,
		},
		{
			name: "non-ErrorResponse error",
			err:  fmt.Errorf("not found"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGitHubNotFound(tt.err); got != tt.want {
				t.Errorf("isGitHubNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSearchSyntaxError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "422 with search is invalid in message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "The search is invalid. Check the syntax.", nil),
			want: true,
		},
		{
			name: "422 with syntax in message but no search is invalid",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "Query syntax error at position 42", nil),
			want: false,
		},
		{
			name: "422 with unrelated message",
			err:  makeGitHubError(http.StatusUnprocessableEntity, "A pull request already exists", nil),
			want: false,
		},
		{
			name: "404 with search is invalid in message - wrong status code",
			err:  makeGitHubError(http.StatusNotFound, "The search is invalid. Check the syntax.", nil),
			want: false,
		},
		{
			name: "422 with Validation Failed + search is invalid in nested error",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "The search is invalid"},
			}),
			want: true,
		},
		{
			name: "422 with Validation Failed + syntax in nested error but no search is invalid",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "Query syntax error at position 42"},
			}),
			want: false,
		},
		{
			name: "422 with Validation Failed + mixed-case Invalid Syntax in nested error",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "The search query has Invalid Syntax"},
			}),
			want: true,
		},
		{
			name: "422 with Validation Failed + 'contains invalid syntax' in nested error",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "The search query contains invalid syntax."},
			}),
			want: true,
		},
		{
			name: "422 with 'invalid syntax' in non-search context should not match",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "The field value has invalid syntax"},
			}),
			want: false,
		},
		{
			name: "422 with Validation Failed + unrelated nested error",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "Something else went wrong"},
			}),
			want: false,
		},
		{
			name: "non-ErrorResponse error",
			err:  fmt.Errorf("invalid syntax"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSearchSyntaxError(tt.err); got != tt.want {
				t.Errorf("isSearchSyntaxError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBodyMatchesMergeRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		iid  int64
		want bool
	}{
		{"format 2 bare number", "**GitLab MR Number** | 42 |", 42, true},
		{"format 3 linked number", "**GitLab MR Number** | [42](https://gitlab.example.com/g/p/merge_requests/42) |", 42, true},
		{"wrong MR number", "**GitLab MR Number** | 99 |", 42, false},
		{"empty body", "", 42, false},
		{"partial match no trailing pipe", "**GitLab MR Number** | 42", 42, false},
		{"substring number mismatch", "**GitLab MR Number** | 421 |", 42, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bodyMatchesMergeRequest(tt.body, tt.iid); got != tt.want {
				t.Errorf("bodyMatchesMergeRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}
