# Design Doc: Repository Health Standards

## Status

Proposed

## Context

With the transition to a polyglot monorepo containing Go, Python, and Markdown files, we need a unified approach to code quality. Without standardized linting, formatting, and Continuous Integration (CI) pipelines, the codebase will quickly degrade in consistency and reliability across different languages.

## Goals

- **Consistency**: Enforce standard formatting and linting rules across all supported languages.
- **Shift-Left Quality**: Catch errors early by running checks locally before commits.
- **Automated Enforcement**: Guarantee repository health by enforcing checks remotely via GitHub Actions.
- **Developer Experience**: Keep the tooling fast, modern, and easy to run.
- **Discoverability**: All automation config must be heavily documented, especially in the `README.md`. "Magic" automation is useless if maintainers don't understand how to use or modify it.

## Proposal

### 1. Tool Selection

We will adopt the following best-in-class tools for each language:

- **Go**:
  - Linter: `golangci-lint` (comprehensive wrapper for Go linters).
  - Formatter: `gofmt` or `gofumpt` (stricter gofmt).
- **Python**:
  - Linter & Formatter: `ruff` (extremely fast, replaces black, isort, and flake8).
  - Type Checker: `mypy` (for static type enforcement).
- **Markdown & Generic**:
  - Formatter/Linter: `prettier` or `markdownlint`.
  - Trailing whitespace and end-of-file fixers.

### Justification & References

Why these specific tools? They represent the current state-of-the-art for performance and modern developer experience:

- **[Ruff (Python)](https://docs.astral.sh/ruff/)**: Written in Rust, Ruff replaces dozens of legacy Python tools (`flake8`, `black`, `isort`, `pydocstyle`, etc.) in a single binary. It is widely benchmarked as being **10-100x faster** than existing linters, executing in milliseconds even on massive codebases like CPython or Pandas.
- **[golangci-lint (Go)](https://golangci-lint.run/)**: This is the de-facto standard in the Go ecosystem. It is exceptionally fast because it runs linters in parallel, heavily caches build and analysis results, and reuses the Go compiler's internal AST representations instead of parsing files multiple times.
- **[pre-commit (Framework)](https://pre-commit.com/)**: A modern, language-agnostic hook manager. It automatically manages isolated environments for each hook (e.g., automatically downloading the `ruff` binary without cluttering the global Python environment) and only runs checks against files modified in the current commit, making it near-instant.

### 2. Git Pre-commit Hooks

We will use the [pre-commit](https://pre-commit.com/) framework to run these tools automatically before code is committed locally.

- A `.pre-commit-config.yaml` file at the root will define the hook configurations.
- Developers will only need to run `pre-commit install` once to set up the local git hook.
- When `git commit` is run, the modified files will be automatically formatted and linted.

### 3. GitHub Actions (CI)

We will establish a CI pipeline using GitHub Actions to enforce these standards on all pull requests and pushes to the main branch.
The `.github/workflows/ci.yml` pipeline will:

1. Checkout the repository.
2. Set up the required Go and Python environments.
3. Run `pre-commit run --all-files` to ensure all code adheres to the linting/formatting standards.
4. Run the language-specific test suites via `task test`.

### 4. Taskfile Integration (Bootstrapping)

To ensure a seamless developer experience and solve the "works on my machine" problem, we will expose the entire setup and execution process through our existing `Taskfile.yml`:

- `task setup`: A command to bootstrap the environment. It will ensure `pre-commit` is installed and configure the local git hooks (`pre-commit install`).
- `task check` or `task lint`: A wrapper to run `pre-commit run --all-files`.
- This guarantees that any new contributor can clone the repository, run `task setup`, and immediately have a fully configured, healthy environment.

### 5. Documentation First

Because automation can often feel like a black box, every layer of configuration must be explicitly documented:

- **README Discoverability**: We will maintain a "Getting Started" section in the root `README.md` that demystifies what `task setup` does behind the scenes, ensuring any developer understands how the git hooks and CI work together.
- **Inline Config Guidance**: Configuration files (e.g., `.github/workflows/ci.yml`, `.pre-commit-config.yaml`, `Taskfile.yml`) must contain extensive inline comments. These comments should explain _why_ a specific tool, hook, or step exists, what its arguments do, and provide hints on how a maintainer might modify or troubleshoot it. The config files should act as their own user manuals.

## Alternatives Considered

While evaluating hook managers, the following alternatives to `pre-commit` were considered:

- **[Lefthook](https://github.com/evilmartians/lefthook)**: Written in Go. It is extremely fast due to parallel execution. However, it requires developers to manage their own local environments (e.g., ensuring `ruff` is installed locally), whereas `pre-commit` automatically provisions isolated environments for its hooks.
- **[Husky](https://typicode.github.io/husky/)**: The standard in the JavaScript ecosystem. Rejected because it requires a Node.js environment, which adds unnecessary overhead to a Go/Python repository.
- **Native Git Hooks**: Writing bash scripts in `.git/hooks/`. Rejected because these are not tracked by version control and require manual bootstrap scripts (e.g., symlinking) to distribute across a team.

## Migration

1. **Configuration**: Create `.pre-commit-config.yaml` at the root and populate it with the selected tools.
2. **CI Setup**: Create `.github/workflows/ci.yml`.
3. **Taskfile Update**: Add `lint` and `check` tasks to the root `Taskfile.yml`.
4. **Initial Pass**: Run `pre-commit run --all-files` against the existing codebase and fix any immediate violations (e.g., in the current Go code or Markdown files).
