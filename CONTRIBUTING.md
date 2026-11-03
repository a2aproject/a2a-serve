# How to contribute

We'd love to accept your patches and contributions to this project. This repository holds `a2a-serve`, the agent server run as `a2a serve` through the [A2A CLI](https://github.com/a2aproject/a2a-cli).

## Ways to contribute

Code is only one way to help. The project grows through many kinds of contribution:

- **Request a feature.** Open a [feature request](https://github.com/a2aproject/a2a-serve/issues/new?template=feature-request.yml) to propose a new command or flag.
- **Report a bug.** File a [bug report](https://github.com/a2aproject/a2a-serve/issues/new?template=bug-report.yml) with steps to reproduce, your `a2a-serve` and `a2a` versions, and your OS.
- **Contribute code.** Fix a bug or build a feature — see the workflow below.
- **Improve the docs.** Clarify the README, the guides, or these guidelines.
- **Share feedback.** Tell us how you use the tool, what works, and what's rough — on our [Discord server](https://discord.com/invite/9UpukKSpRN) or in an issue.
- **Help the community.** Answer questions, help reproduce and triage issues, and review pull requests.

## Before you start

- Read our [Code of Conduct](./CODE_OF_CONDUCT.md). By participating, you agree to follow it.
- For a quick chat or to say Hi, reach out on our [Discord server](https://discord.com/invite/9UpukKSpRN).
- For anything actionable, open an [issue](https://github.com/a2aproject/a2a-serve/issues) or [pull request](https://github.com/a2aproject/a2a-serve/pulls).

## Conventional Commits

Commit messages and pull request titles follow the [Conventional Commits](https://www.conventionalcommits.org/) specification. PR titles are lint-checked. Choose the type and scope based on what you changed:

- **Code** — `feat:` for new behavior, `fix:` for bug fixes (e.g., `feat: add a Slack channel`).
- **Doc files** — `docs:` for documentation.

## Contribution process

### Code reviews

We use GitHub pull requests for reviews. See [GitHub Help](https://help.github.com/articles/about-pull-requests/) for how to use them.

### Workflow

1. **Fork** the official repository to your own account.
2. **Sync** your fork with the latest changes from `main`.
3. **Create a feature branch** and make your changes there.
4. **Commit** using a Conventional Commits message.
5. **Open a pull request** from your feature branch to the official repository's `main` branch.
6. **Resolve feedback** — work with reviewers to address comments.

Be patient — reviewing and merging a pull request can take time.

### Pull request tips

A few checks run on every pull request. Follow these tips to pass them:

- Add tests for every new feature.
- Open an issue first, then link it in the PR. For example: `Fixes #123` or `Closes #123`.
- Use Conventional Commits for commit messages and PR titles.
- For code changes, run these before you push:

  ```sh
  go mod tidy -diff
  go build ./...
  go test -race ./...
  golangci-lint run
  ```

### Trying changes end to end

The tests drive a scripted ACP agent, `fakeacp`, which needs no Docker and no API key. Run a local build of the server with it to try a change by hand:

```sh
go build -o /tmp/a2a-serve . && go build -o /tmp/fakeacp ./internal/agentsrv/testdata/fakeacp
export PATH=/tmp:$PATH   # a2a runs the a2a-serve it finds on PATH
cd "$(mktemp -d)"
a2a serve run acp --sandbox local --command /tmp/fakeacp --detach
a2a send -a "$(a2a serve url)" hello   # "ask" requests an approval, "slow" streams until stopped
a2a serve logs
a2a serve stop
```

`internal/agentsrv/testdata/fakeacp/main.go` lists the prompts it understands.

### End-to-end tests

`./e2e` drives the core user journeys the same way: serving and calling an agent, approvals, restarts, pairing callers, agent contacts and webhook nodes. Each test user gets its own `HOME`, set up with `a2a-serve doctor --fix`. The tests build `a2a-serve` and `fakeacp` and install the pinned `a2a` CLI; set `E2E_A2A_CLI` to the path of an `a2a` binary to skip the install:

```sh
go test -tags e2e ./e2e/...
```

Add a test there when a change affects a journey across commands or processes.

## Releases

Releases are cut by [release-please](https://github.com/googleapis/release-please)
from the Conventional Commits on `main`: it keeps a release pull request up to
date, and merging it tags the release. [GoReleaser](https://goreleaser.com)
(`.goreleaser.yaml`) then attaches the binaries for Linux, macOS and Windows
with their checksums, and opens a pull request with the Homebrew formula in the
`a2aproject/a2a-cli` tap. The winget manifest is generated but not published
until Windows is supported. Try the build locally with
`goreleaser release --snapshot --clean --skip=publish`.
