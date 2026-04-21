#!/usr/bin/env bash
# Starts a NEAR sandbox node in Docker for local development and testing.
# The sandbox exposes a JSON-RPC endpoint compatible with the NEAR mainnet API.
#
# Usage:
#   ./harness.sh          # Start the sandbox
#   ./harness.sh stop     # Stop and remove the container
#
set -e

DOCKER_IMAGE="ghcr.io/near/sandbox:latest"
CONTAINER_NAME="near-sandbox"
RPC_PORT="23456"
NODES_ROOT=~/dextest/near

# Stop and clean up.
if [ "$1" = "stop" ]; then
  echo "Stopping NEAR sandbox..."
  docker stop "${CONTAINER_NAME}" 2>/dev/null || true
  docker rm "${CONTAINER_NAME}" 2>/dev/null || true
  echo "Done."
  exit 0
fi

# Pull the image only if not already present.
if ! docker image inspect "${DOCKER_IMAGE}" >/dev/null 2>&1; then
  echo "Pulling NEAR sandbox image..."
  docker pull "${DOCKER_IMAGE}"
else
  echo "NEAR sandbox image already present."
fi

# Remove any existing container with the same name.
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
  echo "Removing existing container..."
  docker stop "${CONTAINER_NAME}" 2>/dev/null || true
  docker rm "${CONTAINER_NAME}" 2>/dev/null || true
fi

# Prepare local directories.
if [ -d "${NODES_ROOT}" ]; then
  rm -rf "${NODES_ROOT}"
fi
mkdir -p "${NODES_ROOT}"

# The image entrypoint is already "near-sandbox --home /root/.near run".
# No extra arguments needed.
echo "Starting NEAR sandbox on port ${RPC_PORT}..."
docker run -d \
  --name "${CONTAINER_NAME}" \
  -p "${RPC_PORT}:3030" \
  "${DOCKER_IMAGE}"

# Wait for RPC to become available.
echo "Waiting for RPC..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:${RPC_PORT}/status >/dev/null 2>&1; then
    echo "NEAR sandbox is ready on http://localhost:${RPC_PORT}"
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "ERROR: NEAR sandbox did not start within 30 seconds."
    docker logs "${CONTAINER_NAME}"
    exit 1
  fi
  sleep 1
done

# The sandbox ships with a pre-funded "test.near" account (1M NEAR, 50K staked).
# The validator key is baked into the image at /root/.near/validator_key.json.

# Build the sendnear tool for funding accounts.
HARNESS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HARNESS_DIR}/../../.." && pwd)"
echo "Building sendnear tool..."
go build -C "${REPO_ROOT}" -o "${NODES_ROOT}/sendnear" ./dex/testing/near/sendnear/

cat > "${NODES_ROOT}/rpc_endpoint.txt" <<EOF
http://localhost:${RPC_PORT}
EOF

cat > "${NODES_ROOT}/info.txt" <<EOF
NEAR Sandbox
  RPC endpoint: http://localhost:${RPC_PORT}
  Master account: test.near (1,000,000 NEAR)
  Validator key: /root/.near/validator_key.json (inside the container)
EOF

# Write helper scripts.

# Query account balance.
# Usage: ./balance <account_id>
cat > "${NODES_ROOT}/balance" <<BALEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ]; then
  echo "Usage: ./balance <account_id>"
  exit 1
fi
curl -sf http://localhost:${RPC_PORT} -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": "1", "method": "query",
  "params": {"request_type": "view_account", "finality": "final", "account_id": "'\"\$1\"'"}
}' | python3 -m json.tool
BALEOF
chmod +x "${NODES_ROOT}/balance"

# Query latest block.
cat > "${NODES_ROOT}/block" <<BLOCKEOF
#!/usr/bin/env bash
curl -sf http://localhost:${RPC_PORT} -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": "1", "method": "block",
  "params": {"finality": "final"}
}' | python3 -m json.tool
BLOCKEOF
chmod +x "${NODES_ROOT}/block"

# Send NEAR from test.near to a recipient.
# Usage: ./send <recipient_account_id> <amount_in_NEAR>
cat > "${NODES_ROOT}/send" <<SENDEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ] || [ -z "\$2" ]; then
  echo "Usage: ./send <recipient> <amount_in_NEAR>"
  exit 1
fi
"${NODES_ROOT}/sendnear" "http://localhost:${RPC_PORT}" "\$1" "\$2"
SENDEOF
chmod +x "${NODES_ROOT}/send"

# Quit script.
cat > "${NODES_ROOT}/quit" <<QUITEOF
#!/usr/bin/env bash
docker stop ${CONTAINER_NAME} 2>/dev/null || true
docker rm ${CONTAINER_NAME} 2>/dev/null || true
echo "NEAR sandbox stopped."
QUITEOF
chmod +x "${NODES_ROOT}/quit"

echo ""
echo "NEAR sandbox harness is running."
echo "  RPC:     http://localhost:${RPC_PORT}"
echo "  Data:    ${NODES_ROOT}"
echo "  Send:    ${NODES_ROOT}/send <recipient> <amount_NEAR>"
echo "  Balance: ${NODES_ROOT}/balance <account_id>"
echo "  Stop:    ${NODES_ROOT}/quit"
echo ""
