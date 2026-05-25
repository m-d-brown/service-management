# GEMINI.md: Project Overview and Guide

## User's instructions

- **Path and Link Safety**: NEVER write absolute system paths (e.g.,
  `/Users/username/...` or similar local absolute paths) or local `file:///`
  URLs into any files that are tracked in version control (such as README.md,
  code files, Taskfiles). Always use relative paths for linking files inside
  git-tracked repository files. Absolute `file:///` URLs are strictly reserved
  for internal agent-facing artifacts (e.g. implementation plans, walkthroughs,
  tasks) located in `.gemini/` directories.
- **Private Information & Leak Prevention**: Proactively look for, warn the user
  about, and prevent private, personal, or sensitive information (e.g., system
  usernames, local directory structures, private network details, API
  credentials, passwords) from being added to files tracked by git in this
  public repository. If any sensitive details are encountered in the workspace
  or logs, sanitize and generalize them using placeholders (like `username`,
  `domain.example`) before saving them into version control.

## Project Overview

This repository is a collection of tools and libraries for managing
infrastructure services (e.g., in homelab environments).

The main technologies used are:

- **Go**: For tools like `container-version-snapshot`.
- **Python**: For tools like `reboot-orchestrator`.
- **Task**: For build orchestration and common commands.
