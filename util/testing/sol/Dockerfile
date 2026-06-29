FROM ubuntu:noble

ARG TARGETARCH
ARG AGAVE_TAG=v4.0.3

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
    bash \
    build-essential \
    ca-certificates \
    clang \
    cmake \
    coreutils \
    curl \
    git \
    jq \
    libclang-dev \
    libssl-dev \
    libudev-dev \
    llvm \
    pkg-config \
    protobuf-compiler \
    python3 \
    zlib1g-dev \
  && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /opt/solana

# Agave publishes Linux x86_64 release binaries, but current releases do not
# ship Linux ARM64 artifacts. On arm64 we build from source on-target so the
# validator can run natively without AVX-related x86 emulation failures.
RUN if [ "${TARGETARCH}" = "amd64" ]; then \
      sh -c "$(curl -sSfL https://release.anza.xyz/stable/install)" --no-modify-path && \
      cp -a /root/.local/share/solana/install/active_release/. /opt/solana/; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
      curl https://sh.rustup.rs -sSf | sh -s -- -y --profile minimal && \
      . /root/.cargo/env && \
      rustup component add rustfmt && \
      git clone --depth 1 --branch "${AGAVE_TAG}" https://github.com/anza-xyz/agave.git /tmp/agave && \
      mkdir -p /tmp/agave-logs && \
      cd /tmp/agave && \
      ./scripts/cargo-install-all.sh /opt/solana > /tmp/agave-logs/build.log 2>&1 || \
        (tail -n 200 /tmp/agave-logs/build.log && exit 1) && \
      rm -rf /tmp/agave; \
    else \
      echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2 && \
      exit 1; \
    fi

ENV PATH="/opt/solana/bin:${PATH}"

ENTRYPOINT ["solana-test-validator"]
