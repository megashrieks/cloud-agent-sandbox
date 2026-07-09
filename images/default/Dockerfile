FROM debian:stable-slim

# Toolchain versions (bump as needed).
ARG GO_VERSION=1.23.4
ARG GRADLE_VERSION=8.10.2
ARG DOTNET_CHANNEL=8.0

# The sandbox runs NON-ROOT with a READ-ONLY root filesystem and capped ephemeral
# storage, so every toolchain cache/home must live under a writable tmpfs path.
# /tmp and /workspace are the only writable mounts at runtime.
ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/tmp/sandbox-home \
    npm_config_cache=/tmp/npm-cache \
    PIP_CACHE_DIR=/tmp/pip-cache \
    # Go
    GOPATH=/tmp/go \
    GOCACHE=/tmp/go-build \
    GOMODCACHE=/tmp/go/pkg/mod \
    # Rust: toolchain (read-only) stays in /opt/rust; the writable package cache
    # goes to /tmp/cargo.
    RUSTUP_HOME=/opt/rust \
    CARGO_HOME=/tmp/cargo \
    # Java / Gradle
    GRADLE_USER_HOME=/tmp/gradle \
    # .NET / NuGet
    DOTNET_CLI_HOME=/tmp/dotnet-home \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    NUGET_PACKAGES=/tmp/nuget \
    PATH=/usr/local/go/bin:/opt/rust/bin:/usr/local/dotnet:/opt/gradle/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/usr/bin:/bin

# --- Base tooling: git, python, node, C toolchain, common CLIs, gh -----------
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
        python3-venv \
        nodejs \
        npm \
        build-essential \
        pkg-config \
        libssl-dev \
        default-jdk \
        maven \
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

# --- Go ----------------------------------------------------------------------
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) goarch="amd64" ;; \
        arm64) goarch="arm64" ;; \
        *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz; \
    /usr/local/go/bin/go version

# --- Rust (rustup + cargo, toolchain in /opt/rust, world-readable) -----------
RUN set -eux; \
    export CARGO_HOME=/opt/rust RUSTUP_HOME=/opt/rust; \
    curl -fsSL https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal --default-toolchain stable; \
    chmod -R a+rX /opt/rust; \
    /opt/rust/bin/rustc --version; \
    /opt/rust/bin/cargo --version

# --- Gradle (Maven is installed via apt above) -------------------------------
RUN set -eux; \
    curl -fsSL "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" -o /tmp/gradle.zip; \
    unzip -q /tmp/gradle.zip -d /opt; \
    mv "/opt/gradle-${GRADLE_VERSION}" /opt/gradle; \
    rm /tmp/gradle.zip; \
    /opt/gradle/bin/gradle --version

# --- .NET SDK ----------------------------------------------------------------
RUN set -eux; \
    curl -fsSL https://dot.net/v1/dotnet-install.sh -o /tmp/dotnet-install.sh; \
    chmod +x /tmp/dotnet-install.sh; \
    /tmp/dotnet-install.sh --channel "${DOTNET_CHANNEL}" --install-dir /usr/local/dotnet; \
    rm /tmp/dotnet-install.sh; \
    chmod -R a+rX /usr/local/dotnet; \
    /usr/local/dotnet/dotnet --info

# --- Ensure login shells (`sh -lc`, used by the orchestrator's exec) keep the
#     toolchain paths. /etc/profile resets PATH and would otherwise drop the
#     custom Go/Rust/Gradle/.NET dirs, so re-add them via profile.d. ------------
RUN printf 'export PATH="/usr/local/go/bin:/opt/rust/bin:/usr/local/dotnet:/opt/gradle/bin:$PATH"\n' \
        > /etc/profile.d/10-sandbox-toolchains.sh \
    && chmod 0644 /etc/profile.d/10-sandbox-toolchains.sh

# --- Non-root user + workspace -----------------------------------------------
RUN useradd --uid 1000 --create-home --shell /bin/sh sandbox \
    && mkdir -p /workspace \
    && chown 1000:1000 /workspace

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER 1000
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
