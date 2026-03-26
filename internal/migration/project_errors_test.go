package migration

import (
	"errors"
	"net/http"
	"testing"

	gogithub "github.com/google/go-github/v84/github"
)

func newGitHubErrorResponse(statusCode int, message string, errs []gogithub.Error) *gogithub.ErrorResponse {
	return &gogithub.ErrorResponse{
		Response: &http.Response{StatusCode: statusCode},
		Message:  message,
		Errors:   errs,
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
			name: "non-GitHub error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "422 with invalid syntax in message",
			err:  newGitHubErrorResponse(http.StatusUnprocessableEntity, "Validation Failed: invalid syntax in query", nil),
			want: true,
		},
		{
			name: "422 with invalid syntax in errors slice",
			err: newGitHubErrorResponse(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "The search query has Invalid Syntax"},
			}),
			want: true,
		},
		{
			name: "422 with unrelated message",
			err:  newGitHubErrorResponse(http.StatusUnprocessableEntity, "Validation Failed: missing field", nil),
			want: false,
		},
		{
			name: "500 wrong status code",
			err:  newGitHubErrorResponse(http.StatusInternalServerError, "invalid syntax", nil),
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
			name: "non-GitHub error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "422 with already exists in message",
			err:  newGitHubErrorResponse(http.StatusUnprocessableEntity, "Validation Failed: A pull request already exists for this branch", nil),
			want: true,
		},
		{
			name: "422 with already exists in errors slice",
			err: newGitHubErrorResponse(http.StatusUnprocessableEntity, "Validation Failed", []gogithub.Error{
				{Message: "A pull request already exists for org:branch"},
			}),
			want: true,
		},
		{
			name: "422 with unrelated message",
			err:  newGitHubErrorResponse(http.StatusUnprocessableEntity, "Validation Failed: missing field", nil),
			want: false,
		},
		{
			name: "500 wrong status code",
			err:  newGitHubErrorResponse(http.StatusInternalServerError, "a pull request already exists", nil),
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
