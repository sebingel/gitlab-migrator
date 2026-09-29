# Contributing

This project uses [GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow). The `main` branch can always be released, and every merge into `main` that changes the binaries creates a new release automatically.

## Workflow

1. Start from the latest `main`: run `git fetch origin` and branch from `origin/main`.
2. Use one branch per change, for example `feat/short-name`, `fix/short-name` or `docs/short-name`.
3. Commit your work. Commit titles are short and in present tense, for example "adds -repo-visibility flag" or "fixes retry on 404".
4. Push the branch and open a pull request into `main`.
5. Wait for the checks: CI (build and vet), Lint, Test and Release (dry run). All of them must pass.
6. If the change needs more than a patch release, or no release at all, add a release label (see below).
7. Merge the pull request with "Create a merge commit". This is the only merge method that the repository allows. The Release workflow then builds the binaries and publishes a GitHub release.
8. Delete the branch.

The repository only allows merge commits. The first line of every merge on `main` is `Merge pull request #<number> from <owner>/<branch>`, and the second paragraph is the pull request title. The Release workflow reads the pull request number from this first line.

## Releases

A push to `main` runs the Release workflow (`.github/workflows/release.yaml`) if it changes one of these files:

* Go code (`**.go`), but not tests (`**_test.go`)
* `go.mod` (this includes the Go version) or `go.sum`
* `.github/scripts/build-release.sh` (build flags, asset names and archive content)
* `LICENSE` (part of every archive)

Other changes, for example documentation, tests, workflows or Dependabot updates of GitHub Actions, create no release. The next release includes them. `README.md` is part of the archives too, but a documentation edit alone does not need a new version.

The workflow:

1. finds the highest `vMAJOR.MINOR.PATCH` tag of the repository,
2. reads the pull request number from the first line of the merge commit and reads its labels through the API,
3. computes the next version,
4. builds `gitlab-migrator` for Linux, macOS and Windows, each for amd64 and arm64,
5. creates the tag and the GitHub release with the archives, a checksums file and generated release notes.

All decisions are in `.github/scripts/release-plan.sh`, and `.github/scripts/release-plan_test.sh` tests them.

Each pull request runs the same workflow as a dry run. The dry run builds all release assets, but it publishes nothing. Its job summary shows the version that the merge would create, and the assets are available as a workflow artifact for 7 days. The dry run does not know the file list above: for a pull request that changes none of these files, it still shows a version, but the merge creates no release, and a release label on it has no effect.

### Version labels

| Label | Result | Example |
|---|---|---|
| no label | patch release | v0.16.0 to v0.16.1 |
| `release:minor` | minor release | v0.16.0 to v0.17.0 |
| `release:major` | major release | v0.16.0 to v1.0.0 |
| `release:skip` | no release | |

* `release:major` wins over `release:minor`.
* `release:skip` wins over all other labels. It is the only skip rule of the Release workflow.
* Set the label before you merge. The workflow reads the labels when the merge arrives on `main`.
* Do not put `[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]` or `[actions skip]` in the pull request title or in any commit message of the pull request, not even as a quote. The push of a merge contains the merge commit (its message contains the title) and all commits of the pull request. GitHub itself starts no push workflow if any of these messages has such a marker, so the merge creates no release and shows no warning. The dry run warns about such a title or commit, and it runs again when the title changes. There is one exception: if the marker is in the last commit of the pull request, GitHub starts no checks for the pull request at all, so there is no dry run and no warning. Checks that do not start are the sign. To fix a commit message, rewrite it (for example with `git rebase`) and force push the branch.

### Pushes that are not a pull request merge

A push to `main` that is not the merge commit of a pull request into `main` of this repository, for example a direct push or a sync from the upstream repository, creates a **patch release** with a warning, because no labels can apply. If you want another version, start a manual release instead of pushing directly.

### Manual release

Open the Actions tab, select the Release workflow, click "Run workflow" on `main` and choose patch, minor or major. Use this when you want a release for changes that did not create one, or another bump than a pull request got. Manual releases only work on `main`, and only if `main` has commits after the latest release.

### When a release run fails or is cancelled

* For a temporary problem, for example a GitHub API error, use "Re-run failed jobs". GitHub allows this for 30 days (the release assets are kept that long), and it only works if no other release was made in between. Otherwise the run stops, and you start a manual release instead.
* If the cause is in the code, merge a fix. That merge creates the release.
* "Re-run all jobs" never publishes the same commit twice: it fails if the commit already has a release tag, or if a newer release exists.
* Release runs wait for each other, but GitHub keeps only one waiting run. If you merge three pull requests within a few minutes, the middle run can be cancelled. If the last run creates a release, that release includes all changes, but it only uses the labels of the last pull request. If the last run creates no release (for example because of `release:skip`), the changes of the cancelled run are not released. In both cases, start a manual release with the right bump, or wait for a release run to finish before you merge the next pull request.

The workflow never moves or reuses an existing tag.

### Release assets

* `gitlab-migrator_<version>_linux_<arch>.tar.gz` and `gitlab-migrator_<version>_darwin_<arch>.tar.gz`
* `gitlab-migrator_<version>_windows_<arch>.zip` with `gitlab-migrator.exe`
* `gitlab-migrator_<version>_checksums.txt` with the SHA-256 checksums of all archives

`<version>` has no leading `v`, for example `0.16.1`. To check a download:

```
sha256sum --check --ignore-missing gitlab-migrator_<version>_checksums.txt
```

On macOS, use `shasum -a 256 --check --ignore-missing` instead.

## Local checks

Before you push, run:

```
go build ./...
go vet ./...
go test ./...
golangci-lint run
```

If you change files in `.github/`, also run:

```
actionlint
bash .github/scripts/release-plan_test.sh
bash .github/scripts/build-release.sh v0.0.0-local dist
```

`release-plan_test.sh` needs bash and jq. `build-release.sh` builds all release assets into `dist/`. It needs bash, go, zip, GNU tar or bsdtar, and sha256sum or shasum. On macOS the built-in bsdtar and shasum work; install zip if it is missing.
