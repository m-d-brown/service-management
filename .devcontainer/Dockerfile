# The Debian base from the Dev Containers project: git, curl, zsh, and a
# non-root "vscode" user. Everything else is installed by post-create.sh from
# the repo's own version files, so no version is pinned here.
FROM mcr.microsoft.com/devcontainers/base:bookworm

USER vscode

# mise's shims put the repo's tools on PATH for every process, not just
# interactive shells — the VS Code Git integration running pre-commit, tasks,
# and extensions that shell out to go or golangci-lint all need them.
# ~/.local/bin is where mise itself and `uv tool install` land.
ENV PATH=/home/vscode/.local/share/mise/shims:/home/vscode/.local/bin:$PATH

# Interactive shells need mise activated as well: /etc/profile rebuilds PATH
# from scratch for login shells, dropping the shims the ENV above added.
RUN curl -fsSL https://mise.run | sh \
    && echo 'eval "$(mise activate bash)"' >> ~/.bashrc \
    && echo 'eval "$(mise activate zsh)"' >> ~/.zshrc

# Claude Code, installed the same way: a single binary in ~/.local/bin. The
# ~/.claude directory is created here rather than by the volume mounted over it,
# so the volume inherits this ownership instead of being created by root.
RUN curl -fsSL https://claude.ai/install.sh | bash \
    && mkdir -p ~/.claude
