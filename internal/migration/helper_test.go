package migration

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestShortenTitle(t *testing.T) {
	ascii40 := strings.Repeat("a", 40)
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "empty", title: "", want: ""},
		{name: "short ascii", title: "short", want: "short"},
		{name: "exactly 40 ascii", title: ascii40, want: ascii40},
		{name: "41 ascii", title: ascii40 + "b", want: ascii40 + "..."},
		// 39 ASCII bytes plus "ä" (2 bytes) end at byte 41: a byte cut at 40 splits the rune.
		{name: "two byte rune across byte 40", title: strings.Repeat("a", 39) + "äbcd", want: strings.Repeat("a", 39) + "ä..."},
		// 40 runes of 3 bytes each (120 bytes) are not longer than 40 runes.
		{name: "40 three byte runes", title: strings.Repeat("日", 40), want: strings.Repeat("日", 40)},
		{name: "41 three byte runes", title: strings.Repeat("日", 41), want: strings.Repeat("日", 40) + "..."},
		// 38 ASCII bytes plus an emoji (4 bytes) end at byte 42.
		{name: "four byte rune across byte 40", title: strings.Repeat("a", 38) + "😀😀😀", want: strings.Repeat("a", 38) + "😀😀..."},
		// 20 runes of 2 bytes are 40 bytes but only 20 runes: no cut.
		{name: "short in runes, 40 bytes", title: strings.Repeat("ü", 20), want: strings.Repeat("ü", 20)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortenTitle(tt.title)
			if got != tt.want {
				t.Errorf("shortenTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("shortenTitle(%q) = %q is not valid UTF-8", tt.title, got)
			}
		})
	}
}
