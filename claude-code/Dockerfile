ARG BUILD_FROM
ARG TARGETARCH

# Stage 1: Build the Go bridge binary
FROM golang:1.22-bookworm AS builder
ARG TARGETARCH
WORKDIR /src
COPY bridge/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -mod=vendor -ldflags="-s -w" -o /claude-bridge .

# Stage 2: Runtime
FROM ${BUILD_FROM}

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    bash \
    curl \
    jq \
    openssh-client \
    ripgrep \
    ca-certificates \
    gosu \
    && rm -rf /var/lib/apt/lists/*

# Create a non-root user for running Claude Code
RUN groupadd -r claude && useradd -r -g claude -m -d /home/claude -s /bin/bash claude

# Install Claude CLI via native installer (as root, then make accessible)
ENV HOME=/root
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/root/.local/bin:$PATH"

# Make claude installation accessible to the non-root user
RUN chmod -R a+rX /root /root/.local/share/claude

# Copy bridge binary from builder
COPY --from=builder /claude-bridge /usr/local/bin/claude-bridge

# Copy static assets
COPY bridge/static/ /usr/local/share/claude-bridge/static/

# Copy entrypoint
COPY run.sh /run.sh
RUN chmod a+x /run.sh

WORKDIR /config

ENTRYPOINT ["/run.sh"]
