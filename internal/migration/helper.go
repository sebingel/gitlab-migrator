package migration

import (
	"fmt"
	"strings"

	gitconfig "github.com/go-git/go-git/v5/config"
)

// Pointer returns a pointer to the given value.
func Pointer[T any](v T) *T {
	return &v
}

// ParseProjectSlugs splits a CSV row into GitLab and GitHub path components.
func ParseProjectSlugs(slugs []string) (gitlabPath []string, githubPath []string, err error) {
	if len(slugs) != 2 {
		return nil, nil, fmt.Errorf("too many fields")
	}

	delimPosition := strings.LastIndex(slugs[0], "/")
	if delimPosition < 0 {
		return nil, nil, fmt.Errorf("invalid GitLab project: %s", slugs[0])
	}
	gitlabPath = []string{
		slugs[0][:delimPosition],
		slugs[0][delimPosition+1:],
	}
	githubPath = strings.Split(slugs[1], "/")

	if len(githubPath) != 2 {
		return nil, nil, fmt.Errorf("invalid GitHub project: %s", slugs[1])
	}

	return gitlabPath, githubPath, nil
}

// ChunkRefSpecs splits a slice of RefSpecs into chunks of the specified size.
func ChunkRefSpecs(items []gitconfig.RefSpec, chunkSize int) [][]gitconfig.RefSpec {
	if chunkSize <= 0 {
		return [][]gitconfig.RefSpec{items}
	}

	var chunks [][]gitconfig.RefSpec
	for i := 0; i < len(items); i += chunkSize {
		end := i + chunkSize
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[i:end])
	}

	return chunks
}
