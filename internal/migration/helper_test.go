package migration

import (
	"reflect"
	"testing"

	gitconfig "github.com/go-git/go-git/v5/config"
)

func TestChunkRefSpecs(t *testing.T) {
	five := []gitconfig.RefSpec{"a", "b", "c", "d", "e"}
	tests := []struct {
		name      string
		items     []gitconfig.RefSpec
		chunkSize int
		want      [][]gitconfig.RefSpec
	}{
		{name: "uneven split", items: five, chunkSize: 2, want: [][]gitconfig.RefSpec{{"a", "b"}, {"c", "d"}, {"e"}}},
		{name: "chunk larger than input", items: five, chunkSize: 10, want: [][]gitconfig.RefSpec{five}},
		{name: "zero size returns one chunk", items: five, chunkSize: 0, want: [][]gitconfig.RefSpec{five}},
		{name: "empty input returns no chunks", items: nil, chunkSize: 2, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChunkRefSpecs(tt.items, tt.chunkSize); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ChunkRefSpecs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseProjectSlugs(t *testing.T) {
	tests := []struct {
		name        string
		slugs       []string
		wantGitlab  []string
		wantGithub  []string
		wantErr     bool
	}{
		{
			name:       "valid slugs",
			slugs:      []string{"mygroup/myproject", "myorg/myrepo"},
			wantGitlab: []string{"mygroup", "myproject"},
			wantGithub: []string{"myorg", "myrepo"},
		},
		{
			name:    "gitlab slug missing slash panics without fix",
			slugs:   []string{"noSlashHere", "myorg/myrepo"},
			wantErr: true,
		},
		{
			name:    "github slug missing slash",
			slugs:   []string{"mygroup/myproject", "noSlashHere"},
			wantErr: true,
		},
		{
			name:    "too few fields",
			slugs:   []string{"mygroup/myproject"},
			wantErr: true,
		},
		{
			name:    "too many fields",
			slugs:   []string{"mygroup/myproject", "myorg/myrepo", "extra"},
			wantErr: true,
		},
		{
			name:    "empty gitlab slug",
			slugs:   []string{"", "myorg/myrepo"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gitlabPath, githubPath, err := ParseProjectSlugs(tt.slugs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseProjectSlugs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if len(gitlabPath) != len(tt.wantGitlab) {
					t.Fatalf("gitlabPath = %v, want %v", gitlabPath, tt.wantGitlab)
				}
				for i, v := range tt.wantGitlab {
					if gitlabPath[i] != v {
						t.Errorf("gitlabPath[%d] = %q, want %q", i, gitlabPath[i], v)
					}
				}
				if len(githubPath) != len(tt.wantGithub) {
					t.Fatalf("githubPath = %v, want %v", githubPath, tt.wantGithub)
				}
				for i, v := range tt.wantGithub {
					if githubPath[i] != v {
						t.Errorf("githubPath[%d] = %q, want %q", i, githubPath[i], v)
					}
				}
			}
		})
	}
}
