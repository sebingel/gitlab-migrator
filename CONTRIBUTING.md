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
* `go.mod` (this includes the `go` and `toolchain` lines) or `go.sum`
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
* Do not put `[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]` or `[actions skip]` in the pull request title or in any commit message of the pull request, not even as a quote. Do not add a `skip-checks: true` (or `skip-checks:true`) trailer to any commit message of the pull request either. The push of a merge contains the merge commit (its message contains the title) and all commits of the pull request. GitHub itself starts no push workflow if any of these messages has such a marker or trailer, so the merge creates no release and shows no warning. The dry run warns about such a title or commit, and it runs again when the title changes. There is one exception: if the marker or trailer is in the last commit of the pull request, GitHub starts no checks for the pull request at all, so there is no dry run and no warning. Checks that do not start are the sign. To fix a commit message, rewrite it (for example with `git rebase`) and force push the branch.

### Pushes that are not a pull request merge

A push to `main` that is not the merge commit of a pull request into `main` of this repository, for example a direct push or a sync from the upstream repository, creates a **patch release** with a warning, because no labels can apply. If you want another version, start a manual release instead of pushing directly.

### Manual release

Open the Actions tab, select the Release workflow, click "Run workflow" on `main` and choose patch, minor or major. Use this when you want a release for changes that did not create one, or another bump than a pull request got. Manual releases only work on `main`, and only if `main` has commits after the latest release.

### When a release run fails or is cancelled

* For a temporary problem, for example a GitHub API error, use "Re-run failed jobs". GitHub allows this for 30 days (the release assets are kept that long), and it only works if no other release was made in between. Otherwise the run stops, and you start a manual release instead.
* If the cause is in the code, merge a fix. That merge creates the release.
* "Re-run all jobs" never publishes the same commit twice: it fails if the commit already has a release tag, or if a newer release exists.
* Release runs wait for each other in a queue (up to 100 waiting runs), and no waiting run is cancelled, so normally every merge is planned on its own, with its own labels. GitHub does not guarantee the order of waiting runs. If the run of a newer merge is released first, the run of the older merge fails with "does not contain the previous release". Its changes are already in the newer release, but its labels did not count. Start a manual release if you need its bump.
* Every push, label change and edit of a pull request (title, description or base branch) starts its own dry run. Dry runs run in parallel and are not cancelled, so they can finish in any order, and several of them can belong to the same commit. The newest run in the Actions list shows the current labels, title and base branch.

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

## Go toolchain

`go.mod` has two Go versions:

* The `go` line (for example `go 1.25.0`) is the minimum Go version for everyone who builds from source. It also sets the language version and the default GODEBUG settings. Change it only on purpose.
* The `toolchain` line (for example `toolchain go1.27.1`) is the Go release that CI and the release binaries use, because `actions/setup-go` reads it (`go-version-file: go.mod`). With the default `GOTOOLCHAIN=auto`, an older local `go` command downloads this release too.

The Go toolchain workflow (`.github/workflows/go-toolchain.yaml`) keeps the `toolchain` line on a supported Go release:

1. Every Wednesday, or when you start it with "Run workflow" on `main`, it compares the `toolchain` line with the stable releases on <https://go.dev/dl/?mode=json>. This list only has the supported Go releases.
2. If a newer release exists, it runs `go get toolchain@<version>` and `go mod tidy` on the branch `go-toolchain/<version>` and opens a pull request into `main`.
3. Patch releases come first: if a newer patch release of the current Go release exists (for example go1.27.4 for go1.27.3), the workflow proposes it, even if a newer minor release exists. So security fixes do not wait for a minor update, which can need more work (see below).
4. Otherwise it proposes the newest release, also a new minor release, for example go1.28.0 (Go calls them major releases). Go supports each of them only until two newer ones exist. The `go` line never changes.
5. There is one branch and one pull request per version. If the pull request of the version it would propose is open, the workflow waits. If it is closed or merged, the workflow does not propose that version again: after a closed patch release it proposes the newest release, and every newer release gets a new pull request. So you can close a pull request to skip a version.
6. Two open toolchain pull requests change the same line, so after you merge one, the other has a conflict. If the other one is for an older version, close it: merging it would go back to an older toolchain, and the workflow does not propose a version again once its pull request is merged (it only shows a warning). Resolve the conflict only for a newer version.
7. Merging the pull request changes `go.mod`, so it creates a release with binaries built by the new Go release.

All decisions are in `.github/scripts/go-toolchain.sh`, and `.github/scripts/go-toolchain_test.sh` tests them.

GitHub can turn off the schedule without an error: scheduled workflows of a public repository stop after 60 days without activity, and in a fork of a public repository (this repository is a fork) they are off by default. Check in the Actions tab that the Go toolchain workflow is enabled, and turn it on again if it is not.

If the workflow pushed a branch but could not open its pull request, use "Re-run failed jobs": the job creates the same commit again, so the push has nothing to do, and the job opens the pull request. If you do not, the next scheduled run opens it, as long as this version is still the one to propose and `go.mod` on the branch selects it. If `go.mod` on the branch selects another version, that run fails and asks you to delete the branch. If a newer release came out in the meantime, the run proposes it on a new branch, and you can delete the old branch.

### Checks of a toolchain pull request

A pull request that a workflow opens with `GITHUB_TOKEN` does not start its workflow runs on its own. The runs wait for an approval instead. Open the pull request and select "Approve workflows to run" in the merge box. The required checks must pass before you merge, as for every pull request.

Without a token, the workflow also needs the repository setting "Allow GitHub Actions to create and approve pull requests" (Settings, Actions, General, Workflow permissions). It is on in this repository. If it is off, the push works, but opening the pull request fails.

To let the checks start on their own, add a token as the repository secret `GO_TOOLCHAIN_TOKEN`:

1. Create a fine-grained personal access token (Settings, Developer settings, Personal access tokens, Fine-grained tokens) for this repository only, with the repository permissions "Contents: Read and write" and "Pull requests: Read and write".
2. Add it in this repository under Settings, Secrets and variables, Actions, as the repository secret `GO_TOOLCHAIN_TOKEN`.
3. Renew it before it expires. The workflow uses the secret whenever it exists. With an expired token the workflow fails, it does not fall back to `GITHUB_TOKEN`. Delete the secret to go back to `GITHUB_TOKEN`.

With the token, the push and the pull request belong to the owner of the token.

### New minor Go releases

golangci-lint stops if it was built with an older Go than the `toolchain` line, for example with "the Go language version (go1.27) used to build golangci-lint is lower than the targeted Go version (1.28.0)". So for a new minor release, the golangci-lint check of the toolchain pull request fails until `version` in `.github/workflows/lint.yaml` names a golangci-lint release that supports it. The [golangci-lint changelog](https://github.com/golangci/golangci-lint/blob/main/CHANGELOG.md) says which release added support for a Go version (for example "go1.27 support" in v2.13.0). Push that change to the branch of the pull request.

Also check:

* `isTransientNetworkError` in `cmd/gitlab-migrator/app.go`. It finds some HTTP/2 errors by their text, and a new minor release can change these texts.
* The Ports section of the release notes. The release binaries are for Linux, macOS and Windows on amd64 and arm64 (see `build-release.sh`). A new minor release can drop an old system version, for example Go 1.27 needs macOS 13 Ventura or later. Users of that version then cannot run the next release, so mention it in the release notes and think about the label `release:minor`. <https://go.dev/wiki/MinimumRequirements> lists the requirements of each Go release.

## Local checks

Before you push, run:

```
go build ./...
go vet ./...
go test ./...
golangci-lint run
```

Use the golangci-lint version from `.github/workflows/lint.yaml`. An older one can stop with an error about the Go version (see [New minor Go releases](#new-minor-go-releases)).

If you change files in `.github/`, also run:

```
actionlint
bash .github/scripts/release-plan_test.sh
bash .github/scripts/go-toolchain_test.sh
bash .github/scripts/build-release.sh v0.0.0-local dist
```

`release-plan_test.sh` and `go-toolchain_test.sh` need bash and jq. `build-release.sh` builds all release assets into `dist/`. It needs bash, go, zip, GNU tar or bsdtar, and sha256sum or shasum. On macOS the built-in bsdtar and shasum work; install zip if it is missing.
