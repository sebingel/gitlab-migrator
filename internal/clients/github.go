// Package clients provides interfaces and implementations for GitHub and GitLab
// API clients, enabling dependency injection and testability.
package clients

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
)

// GitHubClient is the interface for GitHub API operations used by the migrator.
type GitHubClient interface {
	GetBranches(ctx context.Context, owner, repo string) ([]*gogithub.Branch, error)
	GetPullRequest(ctx context.Context, org, repo string, prNumber int) (*gogithub.PullRequest, error)
	GetSearchResults(ctx context.Context, query string) (*gogithub.IssuesSearchResult, error)
	GetUser(ctx context.Context, username string) (*gogithub.User, error)
}

// githubClient is the concrete implementation of GitHubClient.
type githubClient struct {
	gh     *gogithub.Client
	logger hclog.Logger
	cache  *cache
}

// NewGitHubClient creates a new GitHub client with caching support.
func NewGitHubClient(gh *gogithub.Client, logger hclog.Logger) GitHubClient {
	return &githubClient{gh: gh, logger: logger, cache: newCache()}
}

// GetBranches returns the branches for a GitHub repository, using the cache.
func (c *githubClient) GetBranches(ctx context.Context, owner, repo string) ([]*gogithub.Branch, error) {
	cacheToken := fmt.Sprintf("%s/%s", owner, repo)
	result := c.cache.getGithubBranches(cacheToken)
	if result == nil {
		c.logger.Debug("listing branches")
		var err error
		result, _, err = c.gh.Repositories.ListBranches(ctx, owner, repo, nil)
		if err != nil {
			return nil, fmt.Errorf("listing branches: %v", err)
		}
		if result == nil {
			return nil, fmt.Errorf("nil result was returned when listing branches for repo: %s/%s", owner, repo)
		}
		c.logger.Trace("caching GitHub branches", "repo", fmt.Sprintf("%s/%s", owner, repo))
		c.cache.setGithubBranches(cacheToken, result)
	}
	return result, nil
}

// GetPullRequest returns a GitHub pull request, using the cache.
func (c *githubClient) GetPullRequest(ctx context.Context, org, repo string, prNumber int) (*gogithub.PullRequest, error) {
	cacheToken := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	pullRequest := c.cache.getGithubPullRequest(cacheToken)
	if pullRequest == nil {
		c.logger.Debug("retrieving pull request details", "owner", org, "repo", repo, "pr_number", prNumber)
		var err error
		pullRequest, _, err = c.gh.PullRequests.Get(ctx, org, repo, prNumber)
		if err != nil {
			return nil, fmt.Errorf("retrieving pull request: %v", err)
		}
		if pullRequest == nil {
			return nil, fmt.Errorf("nil pull request was returned: %d", prNumber)
		}
		c.logger.Trace("caching pull request details", "owner", org, "repo", repo, "pr_number", prNumber)
		c.cache.setGithubPullRequest(cacheToken, *pullRequest)
	}
	return pullRequest, nil
}

// GetSearchResults returns GitHub issue search results, using the cache.
func (c *githubClient) GetSearchResults(ctx context.Context, query string) (*gogithub.IssuesSearchResult, error) {
	result := c.cache.getGithubSearchResults(query)
	if result == nil {
		c.logger.Debug("performing search", "query", query)
		var err error
		result, _, err = c.gh.Search.Issues(ctx, query, nil)
		if err != nil {
			return nil, fmt.Errorf("performing issue search: %w", err)
		}
		if result == nil {
			return nil, fmt.Errorf("nil search result was returned for query: %s", query)
		}
		c.logger.Trace("caching GitHub search result", "query", query)
		c.cache.setGithubSearchResults(query, *result)
	}
	return result, nil
}

// GetUser returns a GitHub user, using the cache.
func (c *githubClient) GetUser(ctx context.Context, username string) (*gogithub.User, error) {
	user := c.cache.getGithubUser(username)
	if user == nil {
		c.logger.Debug("retrieving user details", "username", username)
		var err error
		if user, _, err = c.gh.Users.Get(ctx, username); err != nil {
			return nil, err
		}
		if user == nil {
			return nil, fmt.Errorf("nil user was returned: %s", username)
		}
		c.logger.Trace("caching GitHub user", "username", username)
		c.cache.setGithubUser(username, *user)
	}
	if user.Type == nil {
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisation: %s", username)
	}
	return user, nil
}

// SearchModder is an http.RoundTripper that enables advanced search on GitHub issue searches.
var _ http.RoundTripper = &SearchModder{}

// SearchModder modifies GitHub API search requests to enable advanced search mode.
type SearchModder struct {
	Base http.RoundTripper
}

func (g *SearchModder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil {
		if strings.HasSuffix(req.URL.Path, "/search/issues") {
			values := req.URL.Query()
			values.Set("advanced_search", "true")
			req.URL.RawQuery = values.Encode()
		}
	}
	return g.Base.RoundTrip(req)
}
