package clients

import (
	"fmt"

	"github.com/hashicorp/go-hclog"
	gogitlab "github.com/xanzy/go-gitlab"
)

// GitLabClient is the interface for GitLab API operations used by the migrator.
type GitLabClient interface {
	GetUser(username string) (*gogitlab.User, error)
}

// gitlabClient is the concrete implementation of GitLabClient.
type gitlabClient struct {
	gl     *gogitlab.Client
	logger hclog.Logger
	cache  *cache
}

// NewGitLabClient creates a new GitLab client with caching support.
func NewGitLabClient(gl *gogitlab.Client, logger hclog.Logger) GitLabClient {
	return &gitlabClient{gl: gl, logger: logger, cache: newCache()}
}

// GetUser returns a GitLab user by username, using the cache.
func (c *gitlabClient) GetUser(username string) (*gogitlab.User, error) {
	user := c.cache.getGitlabUser(username)
	if user == nil {
		c.logger.Debug("retrieving user details", "username", username)
		users, _, err := c.gl.Users.ListUsers(&gogitlab.ListUsersOptions{Username: &username})
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if u != nil && u.Username == username {
				c.logger.Trace("caching GitLab user", "username", username)
				c.cache.setGitlabUser(username, *u)
				return u, nil
			}
		}
		return nil, fmt.Errorf("GitLab user not found: %s", username)
	}
	return user, nil
}
