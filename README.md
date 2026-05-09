# service-management

A collection of tools and libraries for managing infrastructure services. This
repo is particularly relevant for 'homelab' environments that use Podman, Docker,
Ansible and Terraform.

## Tools

### `container-version-snapshot`

A tool to snapshot container versions across multiple hosts via SSH.

**Features:**

- Scans multiple hosts via SSH.
- Detects running containers using `podman`.
- Resolves versions from OCI labels or registry lookup.
- Outputs a structured JSON snapshot.

**Installation:**

Use the project's task runner to install the tool to your `$GOBIN`:

```shell
task go:install
```

**Usage:**

```shell
container-version-snapshot --host user@host1 --sudo-host user@host2
```

**Example Output:**

```json
{
  "timestamp": "2026-05-09T00:00:00Z",
  "targets": [
    {
      "host": "host1",
      "user": "user",
      "sudo": false
    }
  ],
  "containers": [
    {
      "host": "host1",
      "container": "nginx-proxy",
      "image_label": "docker.io/library/nginx:latest",
      "on_host_versioning": {
        "oci": "1.25.3",
        "raw": "sha256:abcd...",
        "other_repo_tags": ["nginx:1.25.3"]
      },
      "remote_registry_info": {
        "version": "1.25.4"
      }
    }
  ]
}
```

## Getting Started / Contributing

This repository uses [Task](https://taskfile.dev/) to orchestrate build operations and [pre-commit](https://pre-commit.com/) to enforce code health (linting and formatting) across all languages.

To set up your local development environment:

1. Clone the repository.
2. Run the bootstrap command:
   ```shell
   task setup
   ```
   _What this does: It installs `pre-commit` and configures the git hooks. Every time you try to `git commit`, it will automatically run tools like `golangci-lint` (for Go) and `ruff` (for Python) to format and lint your code._

You can also run these checks manually at any time without committing:

```shell
task lint
```

Our GitHub Actions CI pipeline will automatically run these same checks on all pull requests to ensure nothing slips through.

## Development

We use [Task](https://taskfile.dev/) as our build tool. Use `task` at the root to manage operations across all languages:

- `task build`: Build all binaries.
- `task test`: Run all tests.
- `task tidy`: Tidy all module dependencies.

## Repository Structure

- `go/`: Go-based tools and libraries.
  - `go/cmd/container-version-snapshot`: Tool to snapshot container versions via SSH.
- `python/`: (Planned) Python-based tools and libraries.
- `design/`: Architectural design documents.
- `skills/`: Repository-specific skills and guides.
