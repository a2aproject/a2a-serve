### General

* This is `a2a-serve`, a command plugin of the [a2a CLI](https://github.com/a2aproject/a2a-cli): the binary `a2a-serve` on PATH can also run as `a2a serve`. Agents, callers and nodes still use the `a2a` CLI (`a2a send`, `--unix-socket`, `credential-helper` with the Bazel credential helper protocol), so keep those contracts compatible with it.
* Keep the CLI package clean, create a file-per-command. Try extracting logic into a different package.
* Ask clarifying questions from the user if details important for the task are missing.
* When working on a bug fix, follow the RED-GREEN-BLUE TDD approach.
* Before reviewing code, read `review/baseline.md` and don't report the findings it lists unless the code changed in a way that makes them reachable or worse.
* When you brief a review subagent, tell it to read `review/baseline.md` too. When a finding is dropped by decision, add it to the baseline with the reason.

### Feature Design

* Work from critical user journeys (CUJs) to the implementation. Start with who the users are and what they want to get done, and derive the commands, settings, messages and code from that.
* Present each journey as a short sketch of the interaction: the commands with their output, the chat messages with their buttons, the `config.yaml` snippet.
* For each decision that changes what users do, present the alternatives with their own sketches and trade-offs, and recommend one.
* Prefer the existing commands, flags and settings to new ones, and list what users have to learn.
* a2a-serve serves private networks of trusted devices. Harden ingress, and keep communication inside the network low-effort. When security and ease of use pull apart, make the default case work with no configuration, and let users narrow it.
* Fix a sandbox escape in the sandbox. Don't take trust away from the user's own devices to compensate.
* Every example in `examples/` keeps working as written. A change that breaks an example step is a regression. Make it only when the user explicitly authorizes it, even for a security fix. CI runs only some examples, so walk the others by hand when a change touches their path.
* Make behavior opt-in when it rewrites what the agent sends or overrides the agent's own configuration, such as proxying its requests.
* Name flags for who or what they affect. Don't reuse a prefix that already means something (`--public-*` refers to the public URL), don't start a flag that takes a value with `--no-`, and don't give a product term a second meaning ("the relay" is the Agent Mesh relay).

### Code Style

* Do not leave comments in the code unless they explain a non-trivial implementation detail or highlight a suboptimally handled edge-case.
* When writing comments, keep them concise and high-level, do not repeat the implementation.
* Prefer early `return`-s and `continue` over deeply nested blocks.
* Never ignore error results `_ = smth.Close()`, errors need to bubble up OR be logged.

### Architecture

* Build components with constructors that take their dependencies explicitly. Don't patch fields after construction, and don't use package globals as test seams.
* Dependencies point down. Lower packages don't import `agentsrv` or `cli`; move a shared helper down instead.
* Give each behavior one source of truth. A setting, a list of live settings, or an operation on a running or stopped agent has one implementation, and the other places derive from it.
* Code that writes the agent directory holds its lock.

### Correctness

* Persist state before you act on it, so a failed write can't repeat or lose the action.
* Clear per-task state on every exit path: success, failure, cancel and restart.
* A partial failure is a failure. Don't report success for work that stopped early.
* Shared mutable state has one owner and one way in. Don't read fields guarded by a lock without taking it.
* Everything kept in memory or in the database has a removal path: a TTL, a cap, or cleanup when its owner goes away.
* A hint or error that names a command must work when the user runs it as printed. Test that path.

### Reviews

* Report each finding as a scenario: when X happens, Y happens, though Z should, because H. Avoid internal names the reader has to look up.
* Skip nitpicks and rare edge cases. Report a finding that breaks a documented journey or a hint users follow, whatever its severity.
* A review covers the commit it read. After large changes, review the final head again before calling the change reviewed.

### Security

* External traffic is always untrusted. Private networks get the trust the access model documents.
* Bound every input from outside: body size, time, count and rate. Limit per caller, and keep in mind that callers behind a tunnel share one address.
* Never log secrets or URLs that may carry them. Redact them before you log or store an error.

### Documentation

* Keep each fact in one place and link to it from others. `README.md` and `docs/index.md` mirror each other: change both together.
* Give each page one audience. Getting started is the shortest path to a working agent, without internals, config file details or troubleshooting. The user guide pages in `docs/guide/` cover what users do. `docs/advanced/architecture.md` stays high level, with no code paths. Wire formats such as headers and signatures go in `docs/reference/`, and contributor material goes in `CONTRIBUTING.md`.
* Write user guide pages for someone who wants to get a task done: the command, the output they see, and a sentence or two on what it does. Leave out what the server checks, refuses or enforces behind the scenes, especially when an error or the command output tells the user anyway. Keep a guide page short enough to scan in a few minutes, around 800 words at most.
* Keep limits, caps, quotas and timeouts in `docs/reference/limits.md`. Guide pages link to it where users are likely to hit a limit, and state no numbers themselves.
* Put mechanisms, such as how the server authenticates mail or routes the sandbox's connections, in `docs/advanced/` when they help readers understand the design, and leave them out otherwise. Advanced pages summarize guarantees and trade-offs; edge cases and enforcement details stay in the code.
* Write commands as `a2a-serve <command>` in docs, examples, skills, CLI help and errors. Only the command plugin section of `docs/reference/a2a-cli.md` shows the `a2a serve` form.
* Don't repeat flag lists in prose; link to the command reference. It is generated from the command definitions, so fix help text there and run `go run ./internal/tools/gendocs`.
* Write short, plain sentences with one idea each. Avoid metaphors, slogans and contrastive constructions ("X, not Y", "X rather than Y", "X instead of Y"); state the positive claim. Use "that" for restrictive clauses, and US English.
* Call the coding agent "the agent" and a2a-serve "the server", and its home "the agent home directory". Don't write "harness" in user-facing text, including CLI help and errors. The doctor check IDs `harness-image` and `harness-command` are the exception: they appear in `doctor -o json` and stay stable. Don't write "agent server" either.
* Open each page with a plain sentence that summarizes it; the site uses it as the page's meta description. Put the pre-1.0 warning only in `README.md` and `docs/index.md`, and mark unreleased features as planned.
* In examples, don't hardcode ports or URLs a reader's setup changes (use `a2a-serve status --url=local`), and don't repeat setup that `a2a-serve doctor --fix` does.
* Write links between `docs/` and `examples/` as GitHub resolves them, and check the site with `site/build.sh`, which fails on broken links.

### Test Writing

* Do not leave comments in tests unless they explain a non-trivial implementation detail. Test name and test case setup should be self-explanatory.
* ALWAYS use existing test files as a reference when generating new tests. Prioritize files in the same package if they exist.
* Write "table-driven tests" when logic can be shared across multiple test cases.
* Prioritize testing observable behavior of exported methods, not the internal state.
* Use `google/go-cmp/cmp` and `cmp.Diff(want, got)` for comparing structs, slices, maps, or complex types. Do NOT use `reflect.DeepEqual` or manual field checks.
* Use existing mocking utilities or create a new utility to keep test code clean.
* Use `t.Parallel()` at the start of test cases.
* Use `receiver.Operation() error = %v, want %v` as a template for printing test error check failures.
* Use `receiver.Operation() = %v, want %v` as a template for printing test error check failures.
* Use `receiver.Operation() wrong result (-want +got) diff = %s` as a template for printing test errors received when using `cmp.Diff`.
* Prefer using `t.Fatalf` over `t.Errorf` unless printing all the failed checks is justified or the method is called not from the main goroutine.

### End-to-end Testing

* Build binaries for manual checks the way releases are built, with `CGO_ENABLED=0`.
* Walk the examples a change touches, step by step as written, and report each step as passed, failed or skipped with the reason.
