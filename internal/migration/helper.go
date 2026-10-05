package migration

import (
	"fmt"
	"slices"
	"strings"
	"time"

	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/sebingel/gitlab-migrator/internal/config"
)

// unknownDate is shown in a migrated pull request or comment when GitLab sends
// no date.
const unknownDate = "_Unknown_"

// formatDate formats t with config.DateFormat, or returns unknownDate when t
// is nil.
func formatDate(t *time.Time) string {
	if t == nil {
		return unknownDate
	}
	return t.Format(config.DateFormat)
}

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
	return slices.Collect(slices.Chunk(items, chunkSize))
}

// maxTitleRunes is the number of characters of a merge request title that are
// kept in the metadata header of a migrated pull request.
const maxTitleRunes = 40

// shortenTitle cuts title to maxTitleRunes characters and appends "..." when it
// is longer. It cuts by runes, not bytes, so a multi-byte character is never
// split and the result is always valid UTF-8.
func shortenTitle(title string) string {
	// A rune takes at least one byte, so this many bytes or fewer is short enough.
	if len(title) <= maxTitleRunes {
		return title
	}
	runes := 0
	for i := range title {
		if runes == maxTitleRunes {
			return title[:i] + "..."
		}
		runes++
	}
	return title
}
