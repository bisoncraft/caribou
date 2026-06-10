FROM ubuntu:noble

# Bump RIPPLED_VERSION in lockstep with the local-binary path's tested version
# in harness.sh. Tested against rippled 2.3.0.
ARG RIPPLED_VERSION=2.3.0-1

RUN export DEBIAN_FRONTEND=noninteractive && \
    apt-get update -y && \
    apt-get install --reinstall ca-certificates apt-transport-https wget gnupg -y && \
    mkdir -p /usr/local/share/keyrings/ && \
    wget -q -O - "https://repos.ripple.com/repos/api/gpg/key/public" \
      | gpg --dearmor > /usr/local/share/keyrings/ripple-key.gpg && \
    echo "deb [signed-by=/usr/local/share/keyrings/ripple-key.gpg] \
https://repos.ripple.com/repos/rippled-deb noble stable" \
      | tee /etc/apt/sources.list.d/ripple.list && \
    apt-get update -y && \
    apt-get install -y --no-install-recommends rippled=${RIPPLED_VERSION} && \
    apt-mark hold rippled && \
    rm -rf /var/lib/apt/lists/*

RUN ln -s /opt/ripple/bin/rippled /usr/bin/rippled

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 5005 6006

ENTRYPOINT ["/entrypoint.sh"]
