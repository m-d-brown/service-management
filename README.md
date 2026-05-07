# container-version-snapshot

A Go tool to snapshot container versions across multiple hosts via SSH.

## Features

- Scans multiple hosts via SSH.
- Detects running containers using `podman`.
- Resolves versions from OCI labels or registry lookup.
- Outputs a structured JSON snapshot.



## Usage

```shell
./bin/container-version-snapshot --host user@host1 --sudo-host user@host2
```

## Development

Use `task` to manage common development operations:

- `task build`: Build the binary.
- `task test`: Run tests.
- `task tidy`: Tidy go modules.
