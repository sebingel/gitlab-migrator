package clients

import (
	"sync"

	gogithub "github.com/google/go-github/v84/github"
	gogitlab "github.com/xanzy/go-gitlab"
)

// cache holds cached API responses for both GitHub and GitLab clients.
type cache struct {
	mu sync.RWMutex

	githubBranches      map[string][]*gogithub.Branch
	githubPullRequests  map[string]gogithub.PullRequest
	githubSearchResults map[string]gogithub.IssuesSearchResult
	githubUsers         map[string]gogithub.User

	gitlabUsers map[string]gogitlab.User
}

func newCache() *cache {
	return &cache{
		githubBranches:      make(map[string][]*gogithub.Branch),
		githubPullRequests:  make(map[string]gogithub.PullRequest),
		githubSearchResults: make(map[string]gogithub.IssuesSearchResult),
		githubUsers:         make(map[string]gogithub.User),
		gitlabUsers:         make(map[string]gogitlab.User),
	}
}

func (c *cache) getGithubBranches(key string) []*gogithub.Branch {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.githubBranches[key]
}

func (c *cache) setGithubBranches(key string, v []*gogithub.Branch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.githubBranches[key] = v
}

func (c *cache) getGithubPullRequest(key string) *gogithub.PullRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.githubPullRequests[key]; ok {
		return &v
	}
	return nil
}

func (c *cache) setGithubPullRequest(key string, v gogithub.PullRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.githubPullRequests[key] = v
}

func (c *cache) getGithubSearchResults(key string) *gogithub.IssuesSearchResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.githubSearchResults[key]; ok {
		return &v
	}
	return nil
}

func (c *cache) setGithubSearchResults(key string, v gogithub.IssuesSearchResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.githubSearchResults[key] = v
}

func (c *cache) getGithubUser(key string) *gogithub.User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.githubUsers[key]; ok {
		return &v
	}
	return nil
}

func (c *cache) setGithubUser(key string, v gogithub.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.githubUsers[key] = v
}

func (c *cache) getGitlabUser(key string) *gogitlab.User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.gitlabUsers[key]; ok {
		return &v
	}
	return nil
}

func (c *cache) setGitlabUser(key string, v gogitlab.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gitlabUsers[key] = v
}
