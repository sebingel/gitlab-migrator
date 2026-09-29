# Contributing

This project uses [GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow). The `main` branch can always be released, and every merge into `main` creates a new release automatically.

## Workflow

1. Start from the latest `main`: run `git fetch origin` and branch from `origin/main`.
2. Use one branch per change, for example `feat/short-name`, `fix/short-name` or `docs/short-name`.
3. Commit your work. Commit titles are short and in present tense, for example "adds -repo-visibility flag" or "fixes retry on 404".
4. Push the branch and open a pull request into `main`.
5. Wait for the checks: CI (build and vet), Lint, Test and Release (dry run). All of them must pass.
6. If the change needs more than a patch release, add a release label (see below).
7. Merge the pull request. The Release workflow then builds the binaries and publishes a GitHub release.
8. Delete the branch.

## Releases

Every push to `main` runs the Release workflow (`.github/workflows/release.yaml`). It:

1. finds the highest `vMAJOR.MINOR.PATCH` tag of the repository,
2. reads the labels of the merged pull request,
3. computes the next version,
4. builds `gitlab-migrator` for Linux, macOS and Windows, each for amd64 and arm64,
5. creates the tag and the GitHub release with the archives, a checksums file and generated release notes.

Each pull request runs the same workflow as a dry run. The dry run builds all release assets, but it publishes nothing. Its job summary shows the version that the merge would create, and the assets are available as a workflow artifact for 7 days.

### Version labels

| Label | Result | Example |
|---|---|---|
| no label | patch release | v0.16.0 to v0.16.1 |
| `release:minor` | minor release | v0.16.0 to v0.17.0 |
| `release:major` | major release | v0.16.0 to v1.0.0 |
| `release:skip` | no release | |

* `release:major` wins over `release:minor`.
* `release:skip` wins over all other labels.
* Instead of `release:skip` you can write `[skip release]` in the merge commit message. The default merge message contains the pull request title, so `[skip release]` in the title works too.
* Set the label before you merge. The workflow reads the labels when the merge arrives on `main`.

### Changes that create no release

* A push that changes only Markdown files (`*.md`) or images (`*.jpeg`). The next release includes these changes. The dry run of such a pull request does not know this rule: its summary still shows a version, and a release label on it has no effect.
* Dependabot updates for GitHub Actions. They get the `release:skip` label automatically, because they do not change the binaries. Updates of Go modules do create a patch release.

### Manual release

Open the Actions tab, select the Release workflow, click "Run workflow" on `main` and choose patch, minor or major. Use this when you want a release for changes that did not create one. Manual releases only work on `main`.

### When a release run fails or is cancelled

* For a temporary problem, for example a GitHub API error, use "Re-run failed jobs". This only works if no other release was made in between. Otherwise the run stops, and you start a manual release instead.
* If the cause is in the code, merge a fix. That merge creates the release.
* "Re-run all jobs" on an old run fails if a newer release exists, because the workflow refuses to release a commit that is older than the latest release. Use a manual release instead.
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

The last command builds all release assets into `dist/`. It needs `zip`.
