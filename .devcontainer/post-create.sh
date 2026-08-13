#!/usr/bin/env bash
# Installs the toolchain from the files that already own the versions —
# mise.toml (uv, task, golangci-lint), .python-version (Python) and go/go.mod
# (Go) — the same files CI reads, so the container cannot drift from it.
# Re-run by hand after bumping any of them:
#
#   ./.devcontainer/post-create.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# The workspace is bind-mounted from the host, so its files can carry a uid
# that does not exist here and git would refuse to touch the repository.
git config --global safe.directory "$PWD"

# mise will not read a config file it has not been told to trust.
mise trust
mise install --yes

# go.mod is the only place the Go version lives; hand it to mise rather than
# restating it here.
mise use --global --yes "go@$(awk '$1 == "go" { print $2; exit }' go/go.mod)"

# task setup runs `pre-commit install`, so pre-commit has to exist first. Its
# own isolated environment keeps it out of the project's virtualenv.
uv tool install pre-commit

# The same bootstrap a native checkout runs: uv sync + pre-commit install.
task setup

# That `uv sync` covers only the workspace root, which has no dependencies of
# its own. Sync the members too, so the interpreter the editor is pointed at
# actually has pytest, ruff and mypy in it.
uv sync --all-packages
