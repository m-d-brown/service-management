---
name: container-version-snapshot-rules
description: Use this skill whenever you are modifying the container-version-snapshot repository or working with its codebase. It contains the primary design principles and rules for the project.
---

# Design Principles for container-version-snapshot

- **Explicit Over Implicit:** Avoid hidden behaviors, such as silently trying to fetch containers via both rootless (`podman`) and rootful (`sudo podman`) contexts. The execution context should be explicit and user-directed. This applies to CLI flags, SSH connections, and any other automated behavior.
