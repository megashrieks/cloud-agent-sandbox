FROM debian:stable-slim

ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/tmp/sandbox-home \
    npm_config_cache=/tmp/npm-cache \
    PIP_CACHE_DIR=/tmp/pip-cache

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        wget \
        gnupg \
        git \
        openssh-client \
        python3 \
        python3-pip \
        nodejs \
        npm \
        build-essential \
        less \
        jq \
        ripgrep \
        grep \
        sed \
        gawk \
        findutils \
        coreutils \
        diffutils \
        patch \
        gzip \
        bzip2 \
        xz-utils \
        zip \
        unzip \
        tar \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --uid 1000 --create-home --shell /bin/sh sandbox \
    && mkdir -p /workspace \
    && chown 1000:1000 /workspace

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER 1000
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
