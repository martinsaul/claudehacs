ARG BUILD_FROM
FROM ${BUILD_FROM}

# Install runtime dependencies
RUN apk add --no-cache \
    git \
    bash \
    curl \
    jq \
    openssh-client \
    ttyd \
    tmux \
    libgcc \
    libstdc++ \
    ripgrep

# Install Claude CLI via native installer
ENV HOME=/root
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/root/.local/bin:$PATH"

# Use system ripgrep (musl-compatible)
ENV USE_BUILTIN_RIPGREP=0

# Copy entrypoint
COPY run.sh /run.sh
RUN chmod a+x /run.sh

WORKDIR /config

ENTRYPOINT ["/run.sh"]
