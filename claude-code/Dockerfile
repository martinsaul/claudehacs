ARG BUILD_FROM=ghcr.io/hassio-addons/base:17.2.4
FROM ${BUILD_FROM}

# Install Node.js and build dependencies
RUN apk add --no-cache \
    nodejs \
    npm \
    git \
    bash \
    curl \
    jq \
    openssh-client \
    ttyd \
    python3 \
    make \
    g++

# Install Claude CLI globally
RUN npm install -g @anthropic-ai/claude-code

# Clean up build dependencies
RUN apk del make g++

# Copy root filesystem
COPY run.sh /run.sh
RUN chmod a+x /run.sh

WORKDIR /config

CMD ["/run.sh"]
