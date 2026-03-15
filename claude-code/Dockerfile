ARG BUILD_FROM
FROM ${BUILD_FROM}

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    bash \
    curl \
    jq \
    openssh-client \
    tmux \
    ripgrep \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install ttyd
RUN curl -fsSL https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64 -o /usr/local/bin/ttyd \
    && chmod +x /usr/local/bin/ttyd

# Create a non-root user for running Claude Code
# (--dangerously-skip-permissions is blocked when running as root)
RUN groupadd -r claude && useradd -r -g claude -m -d /home/claude -s /bin/bash claude

# Install Claude CLI via native installer (as root, then make accessible)
ENV HOME=/root
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/root/.local/bin:$PATH"

# Make claude installation accessible to the non-root user
RUN chmod -R a+rX /root /root/.local/share/claude

# Copy entrypoint
COPY run.sh /run.sh
RUN chmod a+x /run.sh

WORKDIR /config

ENTRYPOINT ["/run.sh"]
