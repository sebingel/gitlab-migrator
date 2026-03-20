package cache

import (
	"sync"

	gogithub "github.com/google/go-github/v84/github"
	gogitlab "github.com/xanzy/go-gitlab"
)

const (
	GithubBranchesCacheType     uint8 = iota
	GithubPullRequestCacheType
	GithubSearchResultsCacheType
	GithubUserCacheType
	GitlabUserCacheType
)

// ObjectCache is a thread-safe in-memory cache for GitHub and GitLab API objects.
type ObjectCache struct {
	mutex *sync.RWMutex
	store map[uint8]map[string]any
}

// NewObjectCache creates a new ObjectCache.
func NewObjectCache() *ObjectCache {
	store := make(map[uint8]map[string]any)
	store[GithubBranchesCacheType] = make(map[string]any)
	store[GithubPullRequestCacheType] = make(map[string]any)
	store[GithubSearchResultsCacheType] = make(map[string]any)
	store[GithubUserCacheType] = make(map[string]any)
	store[GitlabUserCacheType] = make(map[string]any)

	return &ObjectCache{
		mutex: new(sync.RWMutex),
		store: store,
	}
}

func (c ObjectCache) GetGithubBranches(query string) []*gogithub.Branch {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[GithubBranchesCacheType][query]; ok {
		return v.([]*gogithub.Branch)
	}
	return nil
}

func (c ObjectCache) SetGithubBranches(query string, result []*gogithub.Branch) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[GithubBranchesCacheType][query] = result
}

func (c ObjectCache) GetGithubPullRequest(query string) *gogithub.PullRequest {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[GithubPullRequestCacheType][query]; ok {
		pr := v.(gogithub.PullRequest)
		return &pr
	}
	return nil
}

func (c ObjectCache) SetGithubPullRequest(query string, result gogithub.PullRequest) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[GithubPullRequestCacheType][query] = result
}

func (c ObjectCache) GetGithubSearchResults(query string) *gogithub.IssuesSearchResult {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[GithubSearchResultsCacheType][query]; ok {
		r := v.(gogithub.IssuesSearchResult)
		return &r
	}
	return nil
}

func (c ObjectCache) SetGithubSearchResults(query string, result gogithub.IssuesSearchResult) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[GithubSearchResultsCacheType][query] = result
}

func (c ObjectCache) GetGithubUser(username string) *gogithub.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[GithubUserCacheType][username]; ok {
		u := v.(gogithub.User)
		return &u
	}
	return nil
}

func (c ObjectCache) SetGithubUser(username string, user gogithub.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[GithubUserCacheType][username] = user
}

func (c ObjectCache) GetGitlabUser(username string) *gogitlab.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[GitlabUserCacheType][username]; ok {
		u := v.(gogitlab.User)
		return &u
	}
	return nil
}

func (c ObjectCache) SetGitlabUser(username string, user gogitlab.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[GitlabUserCacheType][username] = user
}
