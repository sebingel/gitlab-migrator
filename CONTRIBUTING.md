# Contributing

This project uses [GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow). The `main` branch can always be released, and every merge into `main` that changes the binaries creates a new release automatically.

## Workflow

1. Start from the latest `main`: run `git fetch origin` and branch from `origin/main`.
2. Use one branch per change, for example `feat/short-name`, `fix/short-name` or `docs/short-name`. The prefix gives the pull request a label for the release notes (see [Release notes](#release-notes)).
3. Commit your work. Commit titles are short and in present tense, for example "adds -repo-visibility flag" or "fixes retry on 404".
4. Push the branch and open a pull request into `main`.
5. Wait for the checks: CI (build and vet), Lint, Test, Vulnerabilities (govulncheck) and Release (dry run). All of them must pass. There is one exception: govulncheck can fail because of a vulnerability that the pull request did not cause (see [Vulnerabilities](#vulnerabilities)). The Labeler check can add a label for the release notes (see [Release notes](#release-notes)).
6. Check the labels of the pull request. If the change needs more than a patch release, or no release at all, add a release label (see [Version labels](#version-labels)). The labels also choose the section of the pull request in the release notes (see [Release notes](#release-notes)).
7. Merge the pull request with "Create a merge commit". This is the only merge method that the repository allows. The Release workflow then builds the binaries and publishes a GitHub release.
8. Delete the branch.

The repository only allows merge commits. The first line of every merge on `main` is `Merge pull request #<number> from <owner>/<branch>`, and the second paragraph is the pull request title. The Release workflow reads the pull request number from this first line.

## Releases

A push to `main` runs the Release workflow (`.github/workflows/release.yaml`) if it changes one of these files:

* Go code (`**.go`), but not tests (`**_test.go`)
* `go.mod` (this includes the `go` and `toolchain` lines) or `go.sum`
* `.github/scripts/build-release.sh` (build flags, asset names and archive content)
* `LICENSE` (part of every archive)

Other changes, for example documentation, tests, workflows or Dependabot updates of GitHub Actions, create no release. The next release includes them, and its notes list them. `README.md` is part of the archives too, but a documentation edit alone does not need a new version.

The workflow:

1. finds the highest `vMAJOR.MINOR.PATCH` tag of the repository,
2. reads the pull request number from the first line of the merge commit and reads its labels through the API,
3. computes the next version,
4. builds `gitlab-migrator` for Linux, macOS and Windows, each for amd64 and arm64,
5. creates the tag and the GitHub release with the archives, a checksums file and generated release notes (see [Release notes](#release-notes)).

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
* `release:skip` wins over all other labels. It is the only label that skips a release. The only other skip rule is for a commit that is already released (see [When a release run fails or is cancelled](#when-a-release-run-fails-or-is-cancelled)).
* Set the label before you merge. The workflow reads the labels when the merge arrives on `main`.
* Do not put `[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]` or `[actions skip]` in the pull request title or in any commit message of the pull request, not even as a quote. Do not add a `skip-checks: true` (or `skip-checks:true`) trailer to any commit message of the pull request either. The push of a merge contains the merge commit (its message contains the title) and all commits of the pull request. GitHub itself starts no push workflow if any of these messages has such a marker or trailer, so the merge creates no release and shows no warning. The dry run warns about such a title or commit, and it runs again when the title changes. There is one exception: if the marker or trailer is in the last commit of the pull request, GitHub starts no checks for the pull request at all, so there is no dry run and no warning. Checks that do not start are the sign. To fix a commit message, rewrite it (for example with `git rebase`) and force push the branch.

### Release notes

The release notes are the changelog of this repository. `CHANGELOG.md` is not updated any more. The Release workflow creates every release with notes that GitHub generates (`gh release create --generate-notes`). They list every pull request that was merged since the previous release, also the ones that created no release of their own, for example documentation changes or pull requests with `release:skip`. Each entry is the title of the pull request with its author and a link, so write the title for the users of the tool. Commits that reach `main` without a pull request of this repository, for example a direct push or a sync from the upstream repository, get no entry. Only the "Full Changelog" link at the end of the notes shows them.

`.github/release.yaml` sorts the pull requests into sections by their labels. A pull request goes into the first section of this table that lists one of its labels. The only exception: a pull request with `bug` is never under "New features", so a fix that also has `enhancement` or `release:minor` is under "Bug fixes".

| Section | Labels |
|---|---|
| Breaking changes | `release:major` |
| New features | `release:minor`, `enhancement` (but not with `bug`) |
| Bug fixes | `bug` |
| Documentation | `documentation` |
| Dependencies | `dependencies` |
| Other changes | all other pull requests |

These labels must exist in the repository. Several files use their names, so if you rename one, search the repository for the old name. For example, `enhancement`, `bug` and `documentation` are in `.github/release.yaml`, `.github/scripts/pr-label.sh` and its test, `dependencies` is also in `.github/dependabot.yaml`, and the `release:` labels are also in `.github/scripts/release-plan.sh`.

The labels come from three places:

* The Labeler workflow (`.github/workflows/labeler.yaml`, rules in `.github/scripts/pr-label.sh`) adds one label from the prefix of the branch. Upper and lower case count as the same.
  * `feat/` or `feature/` gets `enhancement`,
  * `fix/`, `bugfix/` or `hotfix/` gets `bug`,
  * `docs/` gets `documentation`,
  * other prefixes, for example `chore/`, `ci/`, `refactor/` or `dependabot/`, get no label.

  It runs when the pull request is opened and after every push, so a run that failed or did not start (GitHub starts none while a pull request has a merge conflict) is repeated with the next push. It adds nothing if the pull request already has `enhancement`, `bug`, `documentation` or `dependencies`, and it never adds a label again that someone removed. It only adds labels, it never removes one, and it never adds a `release:` label. It skips pull requests from forks, because their token cannot add labels. Label them by hand.
* Dependabot adds `dependencies` to its pull requests (`.github/dependabot.yaml`).
* You add labels by hand, for example `release:minor`, or `documentation` for a `chore/` branch that only changes documentation.

To give a pull request another section than the Labeler chose, remove the label of the Labeler and add the other one. One of these four labels that you set before the Labeler runs, for example with `gh pr create --label`, stops it. To list a pull request under "Other changes", just remove the label of the Labeler.

Only the `release:` labels change the version. The other labels only choose the section. For example, a pull request from a `feat/` branch without `release:minor` is listed under "New features", but it creates a patch release.

GitHub reads the labels when it creates the release. So set them before you merge a pull request that creates a release. For a pull request that creates no release, you can still change the labels until the next release.

To see the notes that the next release would get, run the command below. It saves nothing, but it needs write access to the repository.

```
gh api repos/sebingel/gitlab-migrator/releases/generate-notes -f tag_name=<next version> -f target_commitish=main -f previous_tag_name=<latest version> --jq .body
```

### Pushes that are not a pull request merge

A push to `main` that is not the merge commit of a pull request into `main` of this repository, for example a direct push or a sync from the upstream repository, creates a **patch release** with a warning, because no labels can apply. If you want another version, start a manual release instead of pushing directly.

### Manual release

Open the Actions tab, select the Release workflow, click "Run workflow" on `main` and choose patch, minor or major. Use this when you want a release for changes that did not create one, or another bump than a pull request got. Manual releases only work on `main`, and only if `main` has commits after the latest release.

The notes of a manual release list the merged pull requests like any other release, sorted by their labels. For example, a breaking change that was merged without `release:major` is not under "Breaking changes". Edit the notes of the release afterwards if you want to move it. Do not add `release:major` to the merged pull request for this: if the Release run of that merge still waits in the queue, the label changes its version.

### When a release run fails or is cancelled

* For a temporary problem, for example a GitHub API error, use "Re-run failed jobs". GitHub allows this for 30 days (the release assets are kept that long), and it only works if no other release was made in between. Otherwise the run stops, and you start a manual release instead.
* If the cause is in the code, merge a fix. That merge creates the release.
* No run releases the same commit twice. If the commit already has a release tag, a run of a push skips the release: it writes a notice and ends green. A manual run of such a commit fails, because there is nothing new to release.
* GitHub can send two push events for one merge. Then the Release workflow runs twice for the same commit. The first run creates the release, and the second one waits in the queue and then skips the release as described above.
* "Re-run all jobs" never publishes the same commit twice either. It runs with the event of the first run, so on a released commit a push run skips the release and a manual run fails (see above). On a commit without a release tag, it fails if a newer release exists.
* Release runs wait for each other in a queue (up to 100 waiting runs), and no waiting run is cancelled, so normally every merge is planned on its own, with its own labels. GitHub does not guarantee the order of waiting runs. If the run of a newer merge is released first, the run of the older merge fails with "does not contain the previous release". Its changes are already in the newer release, but its labels did not count. Start a manual release if you need its bump.
* Every push, label change and edit of a pull request (title, description or base branch) starts its own dry run. Dry runs run in parallel and are not cancelled, so they can finish in any order, and several of them can belong to the same commit. The newest run in the Actions list shows the current labels, title and base branch. There is one exception: the label that the Labeler workflow adds starts no dry run, so only the next run, for example after a push, shows it. This label is never a `release:` label, so it does not change the version.

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

* The `go` line (for example `go 1.26.0`) is the minimum Go version for everyone who builds from source. It also sets the language version and the default GODEBUG settings. Change it only on purpose.
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

## Vulnerabilities

The Vulnerabilities workflow (`.github/workflows/govulncheck.yaml`) runs [govulncheck](https://go.dev/doc/tutorial/govulncheck) for every pull request, after every push to `main` and every Monday. It compares the Go standard library and the modules in `go.mod` with the [Go vulnerability database](https://vuln.go.dev), once for each release target (Linux, macOS and Windows, each for amd64 and arm64), because the targets use different source files. govulncheck does not filter the database by system here, so an entry for one system can show up for every target. It fails if the code can call a vulnerable function. The log shows each vulnerability, the version that fixes it and an example call path. Vulnerabilities in code that is not called are only counted in the log.

* For a vulnerability in a module:
  1. Save the default GODEBUG settings of the binary, for example in bash: `GOTOOLCHAIN=go1.27.1 go list -f '{{.DefaultGODEBUG}}' ./cmd/gitlab-migrator`. Use the newest [Go release](https://go.dev/dl/) instead of go1.27.1, and use the same release again in step 3, because the output depends on the Go release that runs the command.
  2. Update the module to the fixed version with `go get <module>@<fixed version>`, then run `go mod tidy`. If the fixed version needs a newer Go, `go get` also raises the `go` line in `go.mod`.
  3. If the `go` line changed, run the command from step 1 again. A higher `go` line changes default GODEBUG settings. Look up each changed setting in the [GODEBUG history](https://go.dev/doc/godebug) and list them in the pull request. Then check the `toolchain` line: if it is missing or older than the newest Go release, set it as described in the next point.
* For a vulnerability in the standard library, set the `toolchain` line in `go.mod` to the newest [Go release](https://go.dev/dl/), for example with `go get toolchain@go1.27.1`. This command adds the line if it is missing and the release is newer than the `go` line. The `toolchain` line can name a newer minor version than the `go` line. Do not raise the `go` line for this, because it is the minimum Go version for everyone who builds from source. CI and the Release workflow build with the version of the `toolchain` line (or of the `go` line if there is no `toolchain` line), and govulncheck checks the standard library of this version.

Such an update changes `go.mod` or `go.sum`, so its merge creates a new release with fixed binaries. golangci-lint also reads the Go version from `go.mod` (the `toolchain` line first), and it fails if it was built with an older Go. If the Lint check fails for this reason, update the golangci-lint version in `.github/workflows/lint.yaml` in the same pull request.

New vulnerabilities are published all the time, so govulncheck can fail on a pull request that did not cause the failure. govulncheck is not a required check, so such a pull request can still be merged, but first compare its log with the latest run on `main`: the pull request must not add a finding. Fix the other findings in their own pull request.

## Local checks

Before you push, run:

```
go build ./...
go vet ./...
go test ./...
golangci-lint run
govulncheck ./...
```

Use the golangci-lint version from `.github/workflows/lint.yaml`. An older one can stop with an error about the Go version (see [New minor Go releases](#new-minor-go-releases)).

To install govulncheck, run `go install golang.org/x/vuln/cmd/govulncheck@v1.8.0` in the project directory. This is the same version as in the Vulnerabilities workflow. It needs Go 1.26 or newer to build. If your Go is older, `go install` downloads a newer Go for the build, if `GOTOOLCHAIN` allows it (the default `auto` does, `local` does not). Install it again when your Go or the Go version in `go.mod` changes, because govulncheck cannot check code for a newer Go than the one it was built with. govulncheck checks the standard library of the Go version that `go version` shows in the project directory. If this is not the version from `go.mod`, the results for the standard library differ from CI. It also only scans for your own system. CI scans every release target with `CGO_ENABLED=0`, like the release builds. To scan one target the same way, set these variables, for example in bash: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 govulncheck ./...`.

If you change files in `.github/`, also run:

```
actionlint
bash .github/scripts/release-plan_test.sh
bash .github/scripts/go-toolchain_test.sh
bash .github/scripts/pr-label_test.sh
bash .github/scripts/build-release.sh v0.0.0-local dist
```

`release-plan_test.sh`, `go-toolchain_test.sh` and `pr-label_test.sh` need bash and jq. `build-release.sh` builds all release assets into `dist/`. It needs bash, go, zip, GNU tar or bsdtar, and sha256sum or shasum. On macOS the built-in bsdtar and shasum work; install zip if it is missing.
