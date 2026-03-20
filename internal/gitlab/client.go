// Package glclient provides a GitLab API client wrapper with caching.
package glclient

import (
	"fmt"

	"github.com/hashicorp/go-hclog"
	gogitlab "github.com/xanzy/go-gitlab"
	"github.com/manicminer/gitlab-migrator/internal/cache"
)

// Client is the interface for the GitLab API client wrapper.
type Client interface {
	GetUser(username string) (*gogitlab.User, error)
}

// client is the concrete implementation of Client.
type client struct {
	gl     *gogitlab.Client
	cache  *cache.ObjectCache
	logger hclog.Logger
}

// NewClient creates a new GitLab client wrapper.
func NewClient(gl *gogitlab.Client, c *cache.ObjectCache, logger hclog.Logger) Client {
	return &client{gl: gl, cache: c, logger: logger}
}

// GetUser returns a GitLab user by username, using the cache.
func (c *client) GetUser(username string) (*gogitlab.User, error) {
	user := c.cache.GetGitlabUser(username)
	if user == nil {
		c.logger.Debug("retrieving user details", "username", username)
		users, _, err := c.gl.Users.ListUsers(&gogitlab.ListUsersOptions{Username: &username})
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if u != nil && u.Username == username {
				c.logger.Trace("caching GitLab user", "username", username)
				c.cache.SetGitlabUser(username, *u)
				return u, nil
			}
		}
		return nil, fmt.Errorf("GitLab user not found: %s", username)
	}
	return user, nil
}
