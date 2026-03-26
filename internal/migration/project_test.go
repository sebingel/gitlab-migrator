package migration

import (
	"fmt"
	"net/http"
	"testing"

	gogithub "github.com/google/go-github/v84/github"
)

func makeGitHubError(statusCode int, message string, errors []gogithub.Error) error {
	return &gogithub.ErrorResponse{
		Response: &http.Response{StatusCode: statusCode},
		Message:  message,
		Errors:   errors,
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
			name: "422 with mixed-case Invalid Syntax in nested error but no search is invalid",
			err: makeGitHubError(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "The search query has Invalid Syntax"},
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
		iid  int
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
