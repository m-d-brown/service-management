# Claude Code instructions

## Linting, testing, building

This repo uses [Task](https://taskfile.dev) (`Taskfile.yml`) as its task runner.
**Prefer running `task` targets over invoking the underlying tools manually.**
Do not work out the raw go/golangci-lint/pytest/uv/pre-commit invocation
yourself — run the corresponding `task` target instead. Use `task --list` to
discover what's available.

Common targets:

- `task check` — lint, format-check, type-check, and scan all files
- `task format` — auto-format and fix all source files
- `task test` — run all tests (Go and Python)
- `task build` — build all binaries
- `task install` — install binaries to GOBIN
- `task tidy` — tidy module dependencies
- `task setup` — bootstrap the repository (sync deps and install git hooks)

## Public repository

This repo is public. Never commit private or personal details: local usernames
or absolute paths, private hostnames or network details, real infrastructure
inventories (service lists, versions, image digests), or credentials. Use
invented, generic data in examples and tests.
