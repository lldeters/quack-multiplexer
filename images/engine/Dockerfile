# quackmux engine image (quackmux/duckdb-engine): the DuckDB CLI with
# pre-installed extensions (default: quack, httpfs), tini as PID 1 and the
# quackmux-engine entrypoint, which implements the engine container contract
# of quackmux (see README.md next to this file).
#
#   docker build -t quackmux/duckdb-engine:2.0.0 images/engine
#
# The build context is this directory. For multi-platform builds
# (docker buildx build --platform linux/amd64,linux/arm64 ...) both stages run
# on the target platform, under emulation when cross-building: INSTALL fetches
# the extensions for the platform of the DuckDB binary that runs it.

ARG BASE_IMAGE=debian:bookworm-slim
ARG DUCKDB_VERSION=2.0.0
# Extensions to pre-install: core extensions by name, others as
# name@repository, e.g. "quack httpfs ducklake postgres_scanner h3@community".
ARG DUCKDB_EXTENSIONS="quack httpfs"

# -----------------------------------------------------------------------------
# Stage 1: download the DuckDB CLI and install the extensions.
FROM ${BASE_IMAGE} AS duckdb

ARG DUCKDB_VERSION
ARG DUCKDB_EXTENSIONS
# Set by BuildKit; without it the architecture of this stage is used.
ARG TARGETARCH
# Optional SHA-256 of the release tarball. DUCKDB_SHA256 applies to every
# architecture (single-platform builds); the per-architecture variants allow
# verified multi-platform builds.
ARG DUCKDB_SHA256=
ARG DUCKDB_SHA256_AMD64=
ARG DUCKDB_SHA256_ARM64=
# Base URL of the release downloads (for mirrors).
ARG DUCKDB_RELEASE_URL=https://github.com/duckdb/duckdb/releases/download
# Optional mirror of the extension repository (custom_extension_repository).
ARG DUCKDB_EXTENSION_REPOSITORY=

RUN set -eu; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl; \
    rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    version="${DUCKDB_VERSION#v}"; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
        amd64) sha="${DUCKDB_SHA256:-${DUCKDB_SHA256_AMD64:-}}" ;; \
        arm64) sha="${DUCKDB_SHA256:-${DUCKDB_SHA256_ARM64:-}}" ;; \
        *) echo "unsupported architecture '$arch': DuckDB publishes Linux CLI builds for amd64 and arm64" >&2; exit 1 ;; \
    esac; \
    asset="duckdb-cli-linux-$arch.tar.gz"; \
    url="${DUCKDB_RELEASE_URL%/}/v$version/$asset"; \
    echo "downloading $url"; \
    curl --fail --silent --show-error --location --retry 5 --retry-connrefused --output "/tmp/$asset" "$url"; \
    if [ -n "$sha" ]; then \
        echo "$(echo "$sha" | tr 'A-F' 'a-f')  /tmp/$asset" | sha256sum -c -; \
    else \
        echo "no DUCKDB_SHA256 given, the download is not verified:"; \
        sha256sum "/tmp/$asset"; \
    fi; \
    mkdir /tmp/duckdb-cli; \
    tar -xzf "/tmp/$asset" -C /tmp/duckdb-cli; \
    bin="$(find /tmp/duckdb-cli -type f -name duckdb -print -quit)"; \
    if [ -z "$bin" ]; then echo "$asset contains no duckdb binary" >&2; exit 1; fi; \
    install -m 0755 "$bin" /usr/local/bin/duckdb; \
    rm -rf "/tmp/$asset" /tmp/duckdb-cli; \
    duckdb -version

# Install the extensions into a fixed directory, load each one to verify it,
# and list them in the build log. At runtime the directory is read-only and
# autoinstall is off, so engines also work without internet access.
RUN set -eu; \
    dir=/opt/duckdb/extensions; \
    mkdir -p "$dir"; \
    sql="SET extension_directories = ['$dir'];"; \
    if [ -n "${DUCKDB_EXTENSION_REPOSITORY:-}" ]; then \
        case "$DUCKDB_EXTENSION_REPOSITORY" in *"'"*) echo "DUCKDB_EXTENSION_REPOSITORY must not contain quotes" >&2; exit 1 ;; esac; \
        sql="$sql SET custom_extension_repository = '$DUCKDB_EXTENSION_REPOSITORY';"; \
    fi; \
    load=""; \
    for spec in $DUCKDB_EXTENSIONS; do \
        name="${spec%%@*}"; \
        case "$name" in ''|*[!a-z0-9_]*) echo "invalid extension name in DUCKDB_EXTENSIONS: '$spec'" >&2; exit 1 ;; esac; \
        if [ "$name" = "$spec" ]; then \
            sql="$sql INSTALL $name;"; \
        else \
            repo="${spec#*@}"; \
            case "$repo" in \
                ''|*"'"*) echo "invalid repository in DUCKDB_EXTENSIONS: '$spec'" >&2; exit 1 ;; \
                *[!a-z0-9_]*) sql="$sql INSTALL $name FROM '$repo';" ;; \
                *) sql="$sql INSTALL $name FROM $repo;" ;; \
            esac; \
        fi; \
        load="$load LOAD $name;"; \
    done; \
    echo "$sql$load"; \
    HOME=/tmp duckdb -init /dev/null -bail -c "$sql$load FROM duckdb_extensions() WHERE installed ORDER BY 1;"; \
    chmod -R a+rX,go-w /opt/duckdb

# The entrypoint, the self-test, the authorization macro for shared engines
# and its test cases (line endings normalized for checkouts on Windows).
COPY entrypoint.sh selftest.sh authorize.sql authorize-test.sh authorize-cases.tsv /usr/local/src/quackmux-engine/
RUN set -eu; \
    cd /usr/local/src/quackmux-engine; \
    sed -i 's/\r$//' entrypoint.sh selftest.sh authorize.sql authorize-test.sh authorize-cases.tsv; \
    bash -n entrypoint.sh; \
    bash -n selftest.sh; \
    bash -n authorize-test.sh; \
    install -m 0755 entrypoint.sh /usr/local/bin/quackmux-engine; \
    install -m 0755 selftest.sh /usr/local/bin/quackmux-engine-selftest; \
    install -m 0755 authorize-test.sh /usr/local/bin/quackmux-authorize-test; \
    install -D -m 0644 authorize.sql /usr/local/share/quackmux/authorize.sql; \
    install -D -m 0644 authorize-cases.tsv /usr/local/share/quackmux/authorize-cases.tsv

# -----------------------------------------------------------------------------
# Stage 2: the runtime image.
FROM ${BASE_IMAGE}

ARG BASE_IMAGE
ARG DUCKDB_VERSION
ARG DUCKDB_EXTENSIONS

LABEL org.opencontainers.image.title="quackmux-duckdb-engine" \
      org.opencontainers.image.description="DuckDB ${DUCKDB_VERSION} serving the Quack protocol as an engine of the quackmux gateway" \
      org.opencontainers.image.version="${DUCKDB_VERSION}" \
      org.opencontainers.image.base.name="${BASE_IMAGE}" \
      io.quackmux.duckdb.version="${DUCKDB_VERSION}" \
      io.quackmux.duckdb.extensions="${DUCKDB_EXTENSIONS}"

# User and group 1000 "duckdb" with HOME=/tmp; /data is a mount point for
# database files that the engine user may write to.
RUN set -eu; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates tini; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 1000 duckdb; \
    useradd --uid 1000 --gid 1000 --home-dir /tmp --no-create-home --shell /usr/sbin/nologin duckdb; \
    install -d -m 0750 -o 1000 -g 1000 /data

COPY --from=duckdb /usr/local/bin/duckdb /usr/local/bin/duckdb
COPY --from=duckdb /opt/duckdb/extensions /opt/duckdb/extensions
COPY --from=duckdb /usr/local/bin/quackmux-engine /usr/local/bin/quackmux-engine-selftest /usr/local/bin/quackmux-authorize-test \
     /usr/local/bin/
COPY --from=duckdb /usr/local/share/quackmux /usr/local/share/quackmux

ENV HOME=/tmp
WORKDIR /tmp
USER 1000:1000

# Smoke test of the finished image as the engine user: start, GET /, SIGTERM,
# init and post-start SQL, what a session may do (see selftest.sh).
# ENGINE_SELFTEST=false skips it.
ARG ENGINE_SELFTEST=true
RUN if [ "$ENGINE_SELFTEST" = "true" ]; then \
        quackmux-engine-selftest && find /tmp -mindepth 1 -delete; \
    else \
        echo "skipping the engine self-test (ENGINE_SELFTEST=$ENGINE_SELFTEST)"; \
    fi

EXPOSE 9494
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/quackmux-engine"]
