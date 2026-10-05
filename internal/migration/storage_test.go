package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/sebingel/gitlab-migrator/internal/config"
)

func TestCreateGitStorage_FilesystemWithSubgroupPath(t *testing.T) {
	tests := []struct {
		name       string
		gitlabPath []string
	}{
		{"top level group", []string{"group", "project"}},
		{"subgroup with slash", []string{"group/sub", "project"}},
		{"nested subgroups with slash", []string{"group/sub/deeper", "project"}},
		{"backslash in the path", []string{`group\sub`, `pro\ject`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storageDir := t.TempDir()
			p := &project{
				m: &Migrator{
					cfg: &config.Config{StorageType: "filesystem", StorageDir: storageDir},
				},
				log:        hclog.NewNullLogger(),
				gitlabPath: tt.gitlabPath,
			}

			stor, err := p.createGitStorage()
			if err != nil {
				t.Fatalf("createGitStorage() error = %v", err)
			}
			if stor == nil {
				t.Fatal("createGitStorage() returned nil storage")
			}

			// The directory must be a direct child of StorageDir: a path
			// separator in the name would have made it a nested path.
			if got := filepath.Dir(p.storagePath); got != filepath.Clean(storageDir) {
				t.Errorf("storage path %q is not a direct child of %q", p.storagePath, storageDir)
			}
			if !strings.HasPrefix(filepath.Base(p.storagePath), "gitlab-migrator-") {
				t.Errorf("storage directory name = %q, want prefix gitlab-migrator-", filepath.Base(p.storagePath))
			}
			if _, err := os.Stat(filepath.Join(p.storagePath, ".git")); err != nil {
				t.Errorf(".git directory missing: %v", err)
			}

			p.cleanupStorage()

			if _, err := os.Stat(p.storagePath); !os.IsNotExist(err) {
				t.Errorf("storage directory still exists after cleanup, stat error = %v", err)
			}
			entries, err := os.ReadDir(storageDir)
			if err != nil {
				t.Fatalf("reading storage dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("storage dir has %d leftover entries after cleanup", len(entries))
			}
		})
	}
}
