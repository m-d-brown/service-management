---
name: design-management
description: Use this skill when managing, creating, or modifying architectural design documents within this repository. It must be used when creating features, designing
APIs, fixing bugs, or improving documentation. It enforces the standards for design proposals.
---

# Design Management

This skill covers the lifecycle of design documentation within this repository: reading, applying, creating, and extending design docs.

## Core Philosophy

Design docs are the "source of truth" for architectural intent. They precede implementation for significant changes and are updated as the system evolves.

## 1. Reading Design Docs

- **Location**: All design docs are stored in the `design/` directory.
- **Naming**: Use an index prefix (e.g., `001-`, `002-`) to maintain chronological order and provide an easy reference ID.
- **Verification**: Always cross-reference a design doc with the current implementation to identify technical debt or drift.

## 2. Creating New Design Docs

- **When**: Create a new doc for any change that introduces a new tool, changes the repository structure, or adds a major feature.
- **Template**:
  - **Status**: Proposed, Accepted, Deprecated.
  - **Context**: Why are we doing this?
  - **Goals**: What does success look like?
  - **Proposal**: The technical detail.
  - **Migration**: How do we get there?
- **Review**: Share the proposed doc with collaborators before writing code.

## 3. Applying Design Docs

- **Traceability**: Implementation PRs or commits should reference the design doc ID.
- **Strictness**: Follow the "Migration" section of the doc strictly to ensure consistency.

## 4. Extending and Updating

- **Drift**: If implementation reveals a flaw in the design, update the design doc rather than letting it become stale.
- **Versioning**: For major pivots, create a new design doc (e.g., `005-v2-of-xyz.md`) and mark the old one as Deprecated with a link to the new one.
- **Annotations**: Small updates can be added as "Appendices" or "Notes" at the bottom of the original document.
