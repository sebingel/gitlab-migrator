package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type largeFile struct {
	ObjectHash string
	Size       int64
	Path       string
}

type prepareReport struct {
	CloneURL       string
	TargetURL      string
	DefaultBranch  string
	LargeFiles     []largeFile
	CleanupMethod  string // "remove", "lfs", or "none"
	RepoSizeBefore int64
	RepoSizeAfter  int64
	BatchPush      bool
	BatchCount     int
	Duration       time.Duration
	Error          string
}

// runPrepare orchestrates the prepare mode workflow.
func runPrepare(ctx context.Context, cloneURL, targetURL, largeFileMode string, batchCount int) error {
	start := time.Now()
	report := &prepareReport{
		CloneURL:  cloneURL,
		TargetURL: targetURL,
	}

	defer func() {
		report.Duration = time.Since(start)
		printPrepareReport(report)
	}()

	logger.Info("starting prepare mode", "source", cloneURL, "target", targetURL)

	// Check prerequisites
	pythonCmd, err := checkPreparePrerequisites(ctx, largeFileMode)
	if err != nil {
		report.Error = err.Error()
		return err
	}

	// Clone repository
	repoDir, err := prepareClone(ctx, cloneURL)
	if err != nil {
		report.Error = err.Error()
		return err
	}

	// Detect default branch
	defaultBranch, err := detectDefaultBranch(ctx, repoDir)
	if err != nil {
		report.Error = err.Error()
		return err
	}
	report.DefaultBranch = defaultBranch
	logger.Info("detected default branch", "branch", defaultBranch)

	// Estimate repo size
	repoSizeBefore, err := estimateRepoSize(repoDir)
	if err != nil {
		report.Error = err.Error()
		return err
	}
	report.RepoSizeBefore = repoSizeBefore
	logger.Info("repository size", "size_mb", repoSizeBefore/1024/1024)

	// Scan for large files
	largeFiles, err := scanLargeFiles(ctx, repoDir)
	if err != nil {
		report.Error = err.Error()
		return err
	}
	report.LargeFiles = largeFiles
	logger.Info("large file scan complete", "count", len(largeFiles))
	for _, f := range largeFiles {
		logger.Info("large file", "path", f.Path, "size_mb", f.Size/1024/1024)
	}

	// Decide cleanup strategy
	needsCleanup := len(largeFiles) > 0
	if needsCleanup && largeFileMode == "" {
		err := fmt.Errorf("found %d files >100MB, use -prepare-large-files=remove or -prepare-large-files=lfs", len(largeFiles))
		report.Error = err.Error()
		return err
	}
	if !needsCleanup && largeFileMode != "" {
		logger.Info("no large files found, skipping cleanup")
	}

	// Execute cleanup if needed
	if needsCleanup {
		report.CleanupMethod = largeFileMode

		// Remove origin remote (required by filter-repo)
		if err := removeRemote(ctx, repoDir, "origin"); err != nil {
			report.Error = err.Error()
			return err
		}

		// Execute cleanup
		switch largeFileMode {
		case "remove":
			if err := executeFilterRepo(ctx, repoDir, pythonCmd, largeFiles); err != nil {
				report.Error = err.Error()
				return err
			}
			logger.Info("filter-repo complete", "files_removed", len(uniquePaths(largeFiles)))

		case "lfs":
			if err := executeLFSMigrate(ctx, repoDir); err != nil {
				report.Error = err.Error()
				return err
			}
			logger.Info("lfs migrate complete")

			// Add remote temporarily for LFS push
			if err := addRemote(ctx, repoDir, "origin", targetURL); err != nil {
				report.Error = err.Error()
				return err
			}
			if err := executeLFSPush(ctx, repoDir, "origin"); err != nil {
				report.Error = err.Error()
				return err
			}
			logger.Info("lfs objects pushed")

			// Remove remote again for clean state before configuring target
			if err := removeRemote(ctx, repoDir, "origin"); err != nil {
				report.Error = err.Error()
				return err
			}
		}
	} else {
		report.CleanupMethod = "none"
	}

	// Handle LFS idempotency: if a previous run converted files to LFS pointers
	// but didn't push LFS objects (e.g. interrupted), detect and push them now.
	if !needsCleanup && largeFileMode == "lfs" {
		if lfsConfigured, _ := hasLFSTracking(repoDir); lfsConfigured {
			logger.Info("LFS tracking detected from previous run, pushing LFS objects")
			if err := addRemote(ctx, repoDir, "origin", targetURL); err != nil {
				report.Error = err.Error()
				return err
			}
			if err := executeLFSPush(ctx, repoDir, "origin"); err != nil {
				report.Error = err.Error()
				return err
			}
			if err := removeRemote(ctx, repoDir, "origin"); err != nil {
				report.Error = err.Error()
				return err
			}
		}
	}

	// Configure target remote
	if err := addRemote(ctx, repoDir, "origin", targetURL); err != nil {
		report.Error = err.Error()
		return err
	}
	logger.Info("configured target remote", "url", targetURL)

	// Measure repo size after cleanup
	repoSizeAfter, err := estimateRepoSize(repoDir)
	if err != nil {
		report.Error = err.Error()
		return err
	}
	report.RepoSizeAfter = repoSizeAfter

	// Push to target
	batched, usedBatchCount, err := pushRepo(ctx, repoDir, "origin", defaultBranch, batchCount, repoSizeAfter)
	if err != nil {
		report.Error = err.Error()
		return err
	}
	report.BatchPush = batched
	report.BatchCount = usedBatchCount
	logger.Info("push complete")

	return nil
}

// checkPreparePrerequisites verifies required tools are installed and returns the python command name.
func checkPreparePrerequisites(ctx context.Context, largeFileMode string) (string, error) {
	// Check git >= 2.22.0
	gitVersionRaw, err := runGitCmd(ctx, "", "--version")
	if err != nil {
		return "", fmt.Errorf("git not found: %v", err)
	}
	gitVersion, err := parseToolVersion(gitVersionRaw, "git version ")
	if err != nil {
		return "", fmt.Errorf("parsing git version from %q: %v", gitVersionRaw, err)
	}
	if !versionAtLeast(gitVersion, [3]int{2, 22, 0}) {
		return "", fmt.Errorf("git >= 2.22.0 required, found %d.%d.%d", gitVersion[0], gitVersion[1], gitVersion[2])
	}
	logger.Info("prerequisite check", "tool", "git", "version", fmt.Sprintf("%d.%d.%d", gitVersion[0], gitVersion[1], gitVersion[2]))

	// Find python command
	pythonCmd := ""
	for _, cmd := range []string{"python3", "python"} {
		out, err := runToolCmd(ctx, cmd, "--version")
		if err != nil {
			continue
		}
		ver, err := parseToolVersion(out, "Python ")
		if err != nil {
			continue
		}
		if versionAtLeast(ver, [3]int{3, 6, 0}) {
			pythonCmd = cmd
			logger.Info("prerequisite check", "tool", "python", "command", cmd, "version", fmt.Sprintf("%d.%d.%d", ver[0], ver[1], ver[2]))
			break
		}
	}

	if largeFileMode == "remove" {
		if pythonCmd == "" {
			return "", fmt.Errorf("python >= 3.6 required for -prepare-large-files=remove, but not found")
		}

		// Check for git-filter-repo
		_, err := runToolCmd(ctx, pythonCmd, "-m", "git_filter_repo", "--version")
		if err != nil {
			return "", fmt.Errorf("git-filter-repo not found; install it with: pip install git-filter-repo")
		}
		logger.Info("prerequisite check", "tool", "git-filter-repo", "status", "available")
	}

	if largeFileMode == "lfs" {
		lfsVersionRaw, err := runToolCmd(ctx, "git", "lfs", "version")
		if err != nil {
			return "", fmt.Errorf("git-lfs not found (required for -prepare-large-files=lfs): %v", err)
		}
		logger.Info("prerequisite check", "tool", "git-lfs", "version", strings.TrimSpace(lfsVersionRaw))
	}

	return pythonCmd, nil
}

// parseToolVersion parses a version string like "git version 2.43.0.windows.1" into [2,43,0].
func parseToolVersion(raw, prefix string) ([3]int, error) {
	raw = strings.TrimSpace(raw)
	idx := strings.Index(raw, prefix)
	if idx == -1 {
		return [3]int{}, fmt.Errorf("prefix %q not found in %q", prefix, raw)
	}
	versionStr := raw[idx+len(prefix):]

	// Extract major.minor.patch — stop at first non-version character
	re := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)
	m := re.FindStringSubmatch(versionStr)
	if m == nil {
		return [3]int{}, fmt.Errorf("cannot parse version from %q", versionStr)
	}

	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return [3]int{major, minor, patch}, nil
}

// versionAtLeast returns true if ver >= min.
func versionAtLeast(ver, min [3]int) bool {
	for i := 0; i < 3; i++ {
		if ver[i] > min[i] {
			return true
		}
		if ver[i] < min[i] {
			return false
		}
	}
	return true // equal
}

// runGitCmd runs a git command, captures output, and logs at DEBUG.
func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	logger.Debug("running git command", "args", args, "dir", dir)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		return result, fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, result)
	}
	logger.Debug("git command output", "args", args, "output", result)
	return result, nil
}

// runGitCmdStreaming runs a git command with stdout/stderr piped to the logger.
func runGitCmdStreaming(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	logger.Info("running git command", "args", args, "dir", dir)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		return fmt.Errorf("starting git %s: %v", strings.Join(args, " "), err)
	}

	// Scan in goroutine so we can close the pipe writer after Wait
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			logger.Info("git", "output", scanner.Text())
		}
	}()

	err := cmd.Wait()
	pw.Close() // unblocks scanner
	<-scanDone // wait for scanner to finish

	if err != nil {
		return fmt.Errorf("git %s: %v", strings.Join(args, " "), err)
	}
	return nil
}

// runToolCmd runs an arbitrary command, captures output.
func runToolCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	logger.Debug("running command", "name", name, "args", args)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		return result, fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, result)
	}
	return result, nil
}

// prepareClone clones the repository as a mirror. Idempotent: skips if directory exists.
func prepareClone(ctx context.Context, cloneURL string) (string, error) {
	// Derive directory name from URL
	repoDir := repoNameFromURL(cloneURL)
	if repoDir == "" {
		return "", fmt.Errorf("cannot derive repository directory name from URL: %s", cloneURL)
	}

	// Resolve to absolute path so all subsequent operations use a stable path
	absDir, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolving clone directory path: %v", err)
	}
	repoDir = absDir

	// Check if directory already exists (idempotent)
	if info, err := os.Stat(repoDir); err == nil && info.IsDir() {
		logger.Info("using existing clone", "dir", repoDir)
		return repoDir, nil
	}

	logger.Info("cloning repository", "url", cloneURL, "dir", repoDir)
	if err := runGitCmdStreaming(ctx, "", "clone", "--mirror", cloneURL, repoDir); err != nil {
		return "", fmt.Errorf("cloning repository: %v", err)
	}
	logger.Info("cloned repository", "dir", repoDir)
	return repoDir, nil
}

// repoNameFromURL extracts a directory name like "repo.git" from a clone URL.
func repoNameFromURL(cloneURL string) string {
	// Handle both https://host/group/repo.git and git@host:group/repo.git
	s := cloneURL
	// Remove trailing slash
	s = strings.TrimRight(s, "/")
	// Get the last path segment
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	} else if idx := strings.LastIndex(s, ":"); idx >= 0 {
		s = s[idx+1:]
	}
	// Ensure it ends with .git
	if !strings.HasSuffix(s, ".git") {
		s = s + ".git"
	}
	return s
}

// scanLargeFiles finds all blobs >100MB in the repository history.
func scanLargeFiles(ctx context.Context, repoDir string) ([]largeFile, error) {
	logger.Info("scanning for large files (>100MB)")

	// Use rev-list piped to cat-file
	revListCmd := exec.CommandContext(ctx, "git", "rev-list", "--objects", "--all")
	revListCmd.Dir = repoDir

	catFileCmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check=%(objecttype) %(objectsize) %(objectname) %(rest)")
	catFileCmd.Dir = repoDir

	revListOut, err := revListCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating rev-list pipe: %v", err)
	}
	catFileCmd.Stdin = revListOut

	catFileOut, err := catFileCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating cat-file pipe: %v", err)
	}

	if err := revListCmd.Start(); err != nil {
		return nil, fmt.Errorf("starting rev-list: %v", err)
	}
	if err := catFileCmd.Start(); err != nil {
		return nil, fmt.Errorf("starting cat-file: %v", err)
	}

	const threshold = 100 * 1024 * 1024 // 100 MB
	var files []largeFile
	scanner := bufio.NewScanner(catFileOut)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			break
		}
		line := scanner.Text()
		// Format: <type> <size> <hash> <path>
		parts := strings.SplitN(line, " ", 4)
		if len(parts) < 4 {
			continue
		}
		objType := parts[0]
		if objType != "blob" {
			continue
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if size > threshold {
			files = append(files, largeFile{
				ObjectHash: parts[2],
				Size:       size,
				Path:       parts[3],
			})
		}
	}

	// Wait for both commands to finish
	if revListErr := revListCmd.Wait(); revListErr != nil {
		return nil, fmt.Errorf("rev-list failed: %v", revListErr)
	}
	if err := catFileCmd.Wait(); err != nil {
		return nil, fmt.Errorf("cat-file failed: %v", err)
	}

	// Sort descending by size
	sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })

	return files, nil
}

// detectDefaultBranch reads HEAD from a bare/mirror clone.
func detectDefaultBranch(ctx context.Context, repoDir string) (string, error) {
	out, err := runGitCmd(ctx, repoDir, "symbolic-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("detecting default branch: %v", err)
	}
	// Strip "refs/heads/" prefix
	branch := strings.TrimPrefix(strings.TrimSpace(out), "refs/heads/")
	return branch, nil
}

// estimateRepoSize walks the directory and sums file sizes.
func estimateRepoSize(repoDir string) (int64, error) {
	var totalSize int64
	err := filepath.Walk(repoDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	return totalSize, err
}

// removeRemote removes a git remote. Idempotent: no-op if remote doesn't exist.
func removeRemote(ctx context.Context, repoDir, name string) error {
	_, err := runGitCmd(ctx, repoDir, "remote", "get-url", name)
	if err != nil {
		// Remote doesn't exist, nothing to do
		logger.Debug("remote does not exist, skipping removal", "remote", name)
		return nil
	}
	logger.Info("removing remote", "remote", name)
	_, err = runGitCmd(ctx, repoDir, "remote", "remove", name)
	return err
}

// addRemote adds a git remote. Idempotent: if remote exists with correct URL, skip; if wrong URL, update.
func addRemote(ctx context.Context, repoDir, name, url string) error {
	existingURL, err := runGitCmd(ctx, repoDir, "remote", "get-url", name)
	if err == nil {
		// Remote exists
		if strings.TrimSpace(existingURL) == url {
			logger.Debug("remote already configured correctly", "remote", name, "url", url)
			return nil
		}
		// Wrong URL, update it
		logger.Info("updating remote URL", "remote", name, "url", url)
		_, err = runGitCmd(ctx, repoDir, "remote", "set-url", name, url)
		return err
	}
	// Remote doesn't exist, add it
	logger.Info("adding remote", "remote", name, "url", url)
	_, err = runGitCmd(ctx, repoDir, "remote", "add", name, url)
	return err
}

// uniquePaths returns deduplicated paths from large files.
func uniquePaths(files []largeFile) []string {
	seen := make(map[string]bool)
	var result []string
	for _, f := range files {
		if !seen[f.Path] {
			seen[f.Path] = true
			result = append(result, f.Path)
		}
	}
	return result
}

// executeFilterRepo runs git-filter-repo to remove large files from history.
func executeFilterRepo(ctx context.Context, repoDir, pythonCmd string, files []largeFile) error {
	paths := uniquePaths(files)
	logger.Info("running git-filter-repo", "paths_to_remove", len(paths))

	args := []string{"-m", "git_filter_repo", "--invert-paths", "--force"}
	for _, p := range paths {
		args = append(args, "--path", p)
	}

	cmd := exec.CommandContext(ctx, pythonCmd, args...)
	cmd.Dir = repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running git-filter-repo: %v", err)
	}
	return nil
}

// executeLFSMigrate runs git lfs fetch --all followed by git lfs migrate import --everything --above=100mb.
func executeLFSMigrate(ctx context.Context, repoDir string) error {
	logger.Info("fetching all LFS objects")
	if err := runGitCmdStreaming(ctx, repoDir, "lfs", "fetch", "--all"); err != nil {
		return fmt.Errorf("git lfs fetch --all: %v", err)
	}

	logger.Info("running git lfs migrate import")
	if err := runGitCmdStreaming(ctx, repoDir, "lfs", "migrate", "import", "--everything", "--above=100mb"); err != nil {
		return fmt.Errorf("git lfs migrate import: %v", err)
	}
	return nil
}

// executeLFSPush pushes all LFS objects to the remote.
func executeLFSPush(ctx context.Context, repoDir, remote string) error {
	logger.Info("pushing LFS objects", "remote", remote)
	if err := runGitCmdStreaming(ctx, repoDir, "lfs", "push", remote, "--all"); err != nil {
		return fmt.Errorf("git lfs push: %v", err)
	}
	return nil
}

// pushRepo decides between direct and batch push based on repo size.
// Returns whether batch push was used and the batch count.
func pushRepo(ctx context.Context, repoDir, remote, defaultBranch string, batchCount int, repoSize int64) (batched bool, usedBatchCount int, err error) {
	const batchThreshold = 2 * 1024 * 1024 * 1024 // 2 GiB

	if repoSize <= batchThreshold && batchCount == 0 {
		logger.Info("repository size under 2 GiB, using direct push", "size_mb", repoSize/1024/1024)
		return false, 0, directPush(ctx, repoDir, remote)
	}

	if batchCount != 0 && repoSize <= batchThreshold {
		logger.Info("batch push forced by -prepare-batch-count override", "size_mb", repoSize/1024/1024, "batch_count", batchCount)
	}

	// Calculate batch count if not overridden
	if batchCount == 0 {
		repoSizeGB := float64(repoSize) / (1024 * 1024 * 1024)
		batchCount = int(math.Ceil(repoSizeGB * 10))
		if batchCount < 10 {
			batchCount = 10
		}
	}

	logger.Info("repository size over 2 GiB, using batch push", "size_mb", repoSize/1024/1024, "batch_count", batchCount)
	return true, batchCount, batchPush(ctx, repoDir, remote, defaultBranch, batchCount)
}

// directPush performs a git push --mirror.
func directPush(ctx context.Context, repoDir, remote string) error {
	return runGitCmdStreaming(ctx, repoDir, "push", "--mirror", remote)
}

// batchPush pushes commits in batches with adaptive retry on push-too-large errors.
// NOTE: Only the default branch is batched. Other branches are pushed via "git push --all"
// in a single operation, which may fail for repos with very large non-default branches.
func batchPush(ctx context.Context, repoDir, remote, branch string, batchCount int) error {
	// Get commit list
	out, err := runGitCmd(ctx, repoDir, "rev-list", "--reverse", "--first-parent", branch)
	if err != nil {
		return fmt.Errorf("listing commits: %v", err)
	}

	commits := strings.Split(strings.TrimSpace(out), "\n")
	if len(commits) == 0 || (len(commits) == 1 && commits[0] == "") {
		return fmt.Errorf("no commits found on branch %s (empty repository? use direct push instead of batch)", branch)
	}

	batchSize := int(math.Ceil(float64(len(commits)) / float64(batchCount)))
	if batchSize < 1 {
		batchSize = 1
	}

	logger.Info("batch push starting", "total_commits", len(commits), "batch_size", batchSize, "batch_count", batchCount)

	lastSuccessIdx := -1
	i := batchSize - 1

	for i < len(commits) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context canceled: %v", err)
		}

		sha := commits[i]
		ref := fmt.Sprintf("%s:refs/heads/%s", sha, branch)
		_, pushErr := runGitCmd(ctx, repoDir, "push", remote, ref)

		if pushErr != nil {
			if isPushTooLargeError(pushErr.Error()) {
				if batchSize <= 1 {
					return fmt.Errorf("single commit exceeds GitHub 2 GiB push limit: %s", sha)
				}
				batchSize = batchSize / 2
				if batchSize < 1 {
					batchSize = 1
				}
				logger.Warn("push rejected (payload too large), halving batch size",
					"new_batch_size", batchSize, "retrying_from", lastSuccessIdx+1)
				i = lastSuccessIdx + batchSize
				continue
			}
			return fmt.Errorf("pushing batch to %s: %v", remote, pushErr)
		}

		lastSuccessIdx = i
		logger.Info("batch pushed", "commits", i+1, "total", len(commits))
		i += batchSize
	}

	// Push tip of default branch (may be past last batch boundary)
	logger.Info("pushing branch tip", "branch", branch)
	if _, err := runGitCmd(ctx, repoDir, "push", remote, branch); err != nil {
		return fmt.Errorf("pushing branch tip: %v", err)
	}

	// Push remaining branches
	logger.Info("pushing all branches")
	if _, err := runGitCmd(ctx, repoDir, "push", "--all", remote); err != nil {
		return fmt.Errorf("pushing all branches: %v", err)
	}

	// Push tags
	logger.Info("pushing tags")
	if _, err := runGitCmd(ctx, repoDir, "push", "--tags", remote); err != nil {
		return fmt.Errorf("pushing tags: %v", err)
	}

	return nil
}

// isAllowedCloneURL validates that a clone URL uses https:// or git@ SSH format.
func isAllowedCloneURL(url string) bool {
	return strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "git@")
}

// hasLFSTracking checks if a repository has LFS filter lines in its attributes.
func hasLFSTracking(repoDir string) (bool, error) {
	content, err := os.ReadFile(filepath.Join(repoDir, "info", "attributes"))
	if err != nil {
		// Also check .gitattributes in the working tree (though bare repos use info/attributes)
		content, err = os.ReadFile(filepath.Join(repoDir, ".gitattributes"))
		if err != nil {
			return false, nil
		}
	}
	return strings.Contains(string(content), "filter=lfs"), nil
}

// isPushTooLargeError detects GitHub's push-too-large error from stderr.
func isPushTooLargeError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "pack exceeds maximum allowed size") ||
		strings.Contains(lower, "413") ||
		strings.Contains(lower, "payload too large")
}

// printPrepareReport prints a final summary of the prepare operation.
func printPrepareReport(r *prepareReport) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("PREPARE SUMMARY")
	fmt.Println(strings.Repeat("=", 80))

	fmt.Printf("Source:          %s\n", r.CloneURL)
	fmt.Printf("Target:          %s\n", r.TargetURL)

	if r.DefaultBranch != "" {
		fmt.Printf("Default Branch:  %s\n", r.DefaultBranch)
	}

	// Large files
	if len(r.LargeFiles) > 0 {
		var totalSize int64
		for _, f := range r.LargeFiles {
			totalSize += f.Size
		}
		fmt.Printf("Large Files:     %d found (%d MB total)\n", len(r.LargeFiles), totalSize/1024/1024)
	} else {
		fmt.Printf("Large Files:     none\n")
	}

	// Cleanup method
	switch r.CleanupMethod {
	case "remove":
		fmt.Printf("Cleanup Method:  remove (git-filter-repo)\n")
		for _, f := range r.LargeFiles {
			fmt.Printf("  Removed:       %s (%d MB)\n", f.Path, f.Size/1024/1024)
		}
	case "lfs":
		fmt.Printf("Cleanup Method:  lfs (git lfs migrate)\n")
	case "none":
		fmt.Printf("Cleanup Method:  none (no large files)\n")
	}

	// Repo size
	if r.RepoSizeBefore > 0 {
		fmt.Printf("Repo Size:       %.1f GB -> %.1f GB\n",
			float64(r.RepoSizeBefore)/(1024*1024*1024),
			float64(r.RepoSizeAfter)/(1024*1024*1024))
	}

	// Push method
	if r.BatchPush {
		fmt.Printf("Push Method:     batch (%d batches, adaptive)\n", r.BatchCount)
	} else if r.Error == "" {
		fmt.Printf("Push Method:     direct (git push --mirror)\n")
	}

	fmt.Printf("Duration:        %s\n", r.Duration.Round(time.Second))

	if r.Error != "" {
		fmt.Printf("Status:          FAILED\n")
		fmt.Printf("Error:           %s\n", r.Error)
	} else {
		fmt.Printf("Status:          SUCCESS\n")
	}

	fmt.Println(strings.Repeat("=", 80))
}
