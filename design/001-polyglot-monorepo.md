# Design Doc: Polyglot Monorepo Architecture

## Status

Proposed

## Context

The `service-management` repository is expanding from a single Go tool
(`container-version-snapshot`) to a suite of tools that may be written in
different languages, specifically Go and Python. To maintain order and
scalability, we need a repository structure that isolates language-specific
environments while providing a unified management interface.

## Goals

- **Environment Isolation**: Prevent language-specific configuration files
  (e.g., `go.mod`, `pyproject.toml`) from cluttering the root.
- **Scalability**: Allow adding new tools in any language without restructuring
  the entire repo.
- **Unified Orchestration**: Provide a single entry point (`Taskfile.yml` at the
  root) for common operations like `build`, `test`, and `lint`.

## Proposed Architecture: Option A (Language-First)

We will adopt a structure where each language has its own top-level directory.

```text
service-management/
├── go/                          # Go ecosystem root
│   ├── cmd/                     # CLI entry points
│   ├── pkg/                     # Internal/Shared libraries
│   └── go.mod                   # Go module definition
├── python/                      # Python ecosystem root
│   ├── src/                     # Library code
│   └── pyproject.toml           # Dependency management (uv/poetry)
├── design/                      # Design documentation
│   └── 001-polyglot-monorepo.md # This document
├── Taskfile.yml                 # Root orchestration
└── README.md                    # Project overview
```

### Migration Plan

1. Move all existing Go files (`cmd/`, `pkg/`, `go.mod`, `go.sum`) into a new
   `go/` directory.
2. Update Go import paths to reflect the new structure (if necessary, though
   `go.mod` inside `go/` might keep them relative to the module root).
3. Create a root `Taskfile.yml` that delegates commands to the
   `go/Taskfile.yml`.
4. Update `README.md` to point to the new locations.

## Alternatives Considered

- **Flat Structure**: Keeping Go at the root and adding a `python/` folder. This
  is easier initially but leads to a messy root directory as the project grows.

## Decision

Adopt Option A (Language-First) for long-term maintainability.
