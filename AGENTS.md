### General

* This is `a2a-serve`, a command plugin of the [a2a CLI](https://github.com/a2aproject/a2a-cli): the binary `a2a-serve` on PATH runs as `a2a serve`. Agents, callers and nodes still use the `a2a` CLI (`a2a send`, `--unix-socket`, `credential-helper` with the Bazel credential helper protocol), so keep those contracts compatible with it.
* Keep the CLI package clean, create a file-per-command. Try extracting logic into a different package.
* Ask clarifying questions from the user if details important for the task are missing.
* When working on a bug fix, follow the RED-GREEN-BLUE TDD approach.

### Code Style

* Do not leave comments in the code unless they explain a non-trivial implementation detail or highlight a suboptimally handled edge-case.
* When writing comments, keep them concise and high-level, do not repeat the implementation.
* Prefer early `return`-s and `continue` over deeply nested blocks.
* Never ignore error results `_ = smth.Close()`, errors need to bubble up OR be logged.

### Documentation

* Keep each fact in one place and link to it from others. `README.md` and `docs/index.md` mirror each other: change both together.
* Give each page one audience. Getting started is the shortest path to a working agent, without internals, config file details or troubleshooting. The user guide covers what users do. `docs/concepts/architecture.md` stays high level, with no code paths. Wire formats such as headers and signatures go in `docs/reference/`, and contributor material goes in `CONTRIBUTING.md`.
* Don't repeat flag lists in prose; link to the command reference. It is generated from the command definitions, so fix help text there and run `go run ./internal/tools/gendocs`.
* Write short, plain sentences with one idea each. Avoid metaphors, slogans and contrastive constructions ("X, not Y", "X rather than Y", "X instead of Y"); state the positive claim. Use "that" for restrictive clauses, and US English.
* Call the coding agent "the agent" and a2a-serve "the server", and its home "the agent home directory". Don't write "harness" in user-facing text, including CLI help and errors.
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