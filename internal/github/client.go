// Package ghclient provides a GitHub API client wrapper with caching.
package ghclient

import (
	"context"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v84/github"
	"github.com/hashicorp/go-hclog"
	"github.com/manicminer/gitlab-migrator/internal/cache"
)

// Client wraps the GitHub API client with caching support.
type Client struct {
	gh     *gogithub.Client
	cache  *cache.ObjectCache
	logger hclog.Logger
}

// NewClient creates a new GitHub client wrapper.
func NewClient(gh *gogithub.Client, c *cache.ObjectCache, logger hclog.Logger) *Client {
	return &Client{gh: gh, cache: c, logger: logger}
}

// Raw returns the underlying GitHub API client for direct API access.
func (c *Client) Raw() *gogithub.Client {
	return c.gh
}

// GetBranches returns the branches for a GitHub repository, using the cache.
func (c *Client) GetBranches(ctx context.Context, owner, repo string) ([]*gogithub.Branch, error) {
	cacheToken := fmt.Sprintf("%s/%s", owner, repo)
	result := c.cache.GetGithubBranches(cacheToken)
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
		c.cache.SetGithubBranches(cacheToken, result)
	}
	return result, nil
}

// GetPullRequest returns a GitHub pull request, using the cache.
func (c *Client) GetPullRequest(ctx context.Context, org, repo string, prNumber int) (*gogithub.PullRequest, error) {
	cacheToken := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	pullRequest := c.cache.GetGithubPullRequest(cacheToken)
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
		c.cache.SetGithubPullRequest(cacheToken, *pullRequest)
	}
	return pullRequest, nil
}

// GetSearchResults returns GitHub issue search results, using the cache.
func (c *Client) GetSearchResults(ctx context.Context, query string) (*gogithub.IssuesSearchResult, error) {
	result := c.cache.GetGithubSearchResults(query)
	if result == nil {
		c.logger.Debug("performing search", "query", query)
		var err error
		result, _, err = c.gh.Search.Issues(ctx, query, nil)
		if err != nil {
			return nil, fmt.Errorf("performing issue search: %v", err)
		}
		if result == nil {
			return nil, fmt.Errorf("nil search result was returned for query: %s", query)
		}
		c.logger.Trace("caching GitHub search result", "query", query)
		c.cache.SetGithubSearchResults(query, *result)
	}
	return result, nil
}

// GetUser returns a GitHub user, using the cache.
func (c *Client) GetUser(ctx context.Context, username string) (*gogithub.User, error) {
	user := c.cache.GetGithubUser(username)
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
		c.cache.SetGithubUser(username, *user)
	}
	if user.Type == nil {
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisation: %s", username)
	}
	return user, nil
}

// SearchModder is an http.RoundTripper that enables advanced search on GitHub issue searches.
var _ http.RoundTripper = &SearchModder{}

type SearchModder struct {
	Base http.RoundTripper
}

func (g *SearchModder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil {
		if req.URL.Path == "/search/issues" {
			values := req.URL.Query()
			values.Set("advanced_search", "true")
			req.URL.RawQuery = values.Encode()
		}
	}
	return g.Base.RoundTrip(req)
}
