FROM debian:stable-slim

ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/tmp/sandbox-home \
    npm_config_cache=/tmp/npm-cache \
    PIP_CACHE_DIR=/tmp/pip-cache

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        gzip \
        jq \
        less \
        openssh-client \
        python3 \
        python3-pip \
        nodejs \
        npm \
        build-essential \
        ripgrep \
        tar \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --uid 1000 --create-home --shell /bin/sh sandbox \
    && mkdir -p /workspace \
    && chown 1000:1000 /workspace

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER 1000
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
