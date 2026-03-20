package migration

import (
	"testing"
)

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
