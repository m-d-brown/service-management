---
name: service-management-rules
description:
  Use this skill whenever you are modifying the service-management repository or
  working with its codebase. It contains the primary design principles and rules
  for the project.
---

# Design Principles for service-management

- **Explicit Over Implicit:** Avoid hidden behaviors, such as silently trying to
  fetch containers via both rootless (`podman`) and rootful (`sudo podman`)
  contexts. The execution context should be explicit and user-directed. This
  applies to CLI flags, SSH connections, and any other automated behavior.
