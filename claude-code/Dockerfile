ARG BUILD_FROM
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

# Copy entrypoint
COPY run.sh /run.sh
RUN chmod a+x /run.sh

WORKDIR /config

ENTRYPOINT ["/run.sh"]
