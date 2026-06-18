#!/usr/bin/env bash
# Verifies Hyperliquid testnet connectivity and sets up the test harness.
# No local node is required — tests run against the public testnet API.
#
# Usage:
#   ./harness.sh        # setup
#   ./harness.sh stop   # no-op (nothing to stop)

set -e

API_URL="https://api.hyperliquid-testnet.xyz"
NODES_ROOT=~/dextest/hyperliquid_testnet

if [ "$1" = "stop" ]; then
  echo "Nothing to stop (testnet harness requires no local process)."
  exit 0
fi

echo "Verifying Hyperliquid testnet connectivity..."
if ! curl -sf -X POST "${API_URL}/info" \
    -H "Content-Type: application/json" \
    -d '{"type":"meta"}' \
    -o /dev/null; then
  echo "ERROR: Hyperliquid testnet API unreachable at ${API_URL}"
  exit 1
fi
echo "Testnet API is reachable."

mkdir -p "${NODES_ROOT}"

HARNESS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HARNESS_DIR}/../../.." && pwd)"

echo "Building sendhype tool..."
go build -C "${REPO_ROOT}" -o "${NODES_ROOT}/sendhype" ./util/testing/hyperliquid_testnet/sendhype/

cat > "${NODES_ROOT}/info.txt" <<EOF
Hyperliquid Testnet Harness
  API endpoint: ${API_URL}
  Funder address: see sendhype/main.go (FUNDER_PRIVATE_KEY)
  Note: The funder account needs testnet HYPE from the HL testnet faucet
        (https://app.hyperliquid-testnet.xyz/faucet or Discord).
EOF

# Helper: query HYPE balance for an address.
cat > "${NODES_ROOT}/balance" <<BALEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ]; then
  echo "Usage: ./balance <0x_address>"
  exit 1
fi
curl -sf -X POST "${API_URL}/info" \
  -H "Content-Type: application/json" \
  -d "{\"type\":\"spotClearinghouseState\",\"user\":\"\$1\"}" | python3 -m json.tool
BALEOF
chmod +x "${NODES_ROOT}/balance"

# Helper: send HYPE from funder to an address.
cat > "${NODES_ROOT}/send" <<SENDEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ] || [ -z "\$2" ]; then
  echo "Usage: ./send <0x_recipient> <amount_hype>"
  exit 1
fi
HYPE_FUNDER_KEY="${HYPE_FUNDER_KEY}" "${NODES_ROOT}/sendhype" "${API_URL}" "\$1" "\$2"
SENDEOF
chmod +x "${NODES_ROOT}/send"

# Helper: stop (no-op).
cat > "${NODES_ROOT}/quit" <<QUITEOF
#!/usr/bin/env bash
echo "Testnet harness has no process to stop."
QUITEOF
chmod +x "${NODES_ROOT}/quit"

echo ""
echo "Hyperliquid testnet harness ready."
echo "  API:     ${API_URL}"
echo "  Data:    ${NODES_ROOT}"
echo "  Send:    ${NODES_ROOT}/send <recipient> <amount_HYPE>"
echo "  Balance: ${NODES_ROOT}/balance <address>"
echo ""
echo "IMPORTANT: The funder account in sendhype/main.go must hold testnet HYPE."
echo "           Obtain from the Hyperliquid testnet faucet if needed."
echo ""
