#!/usr/bin/env bash
# Starts a local Hyperliquid API mock server for wallet testing, with no
# dependency on a funded testnet account.
#
# Usage:
#   ./harness.sh [address-to-fund]   # build, start, optionally pre-fund an address
#   ./harness.sh stop                # stop the running mock server
#
# After setup, point a HYPE wallet at the mock by setting these wallet
# config/settings values (in the desktop wallet, via wallet settings /
# "Reconfigure"):
#   apiendpoint = http://127.0.0.1:18080
#   wsendpoint  = ws://127.0.0.1:18080/ws
#
# Then use the generated helpers in ~/dextest/hyperliquid_mock/:
#   ./send <address> <amount_HYPE>   # credit an address with HYPE (no signing/faucet needed)
#   ./balance <address>              # check HYPE balance
set -e

NODES_ROOT=~/dextest/hyperliquid_mock
ADDR="127.0.0.1:18080"
API_URL="http://${ADDR}"
PIDFILE="${NODES_ROOT}/mockserver.pid"

HARNESS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HARNESS_DIR}/../../.." && pwd)"

if [ "$1" = "stop" ]; then
  if [ -f "${PIDFILE}" ]; then
    kill "$(cat "${PIDFILE}")" 2>/dev/null || true
    rm -f "${PIDFILE}"
    echo "Stopped mock server."
  else
    echo "No running mock server (no pidfile at ${PIDFILE})."
  fi
  exit 0
fi

mkdir -p "${NODES_ROOT}"

echo "Building mockserver..."
go build -C "${REPO_ROOT}" -o "${NODES_ROOT}/mockserver" ./util/testing/hyperliquid_mock/cmd/mockserver/

ARGS=(-addr "${ADDR}")
if [ -n "$1" ]; then
  ARGS+=(-fund-addr "$1" -fund-hype 1000)
fi

"${NODES_ROOT}/mockserver" "${ARGS[@]}" > "${NODES_ROOT}/mockserver.log" 2>&1 &
echo $! > "${PIDFILE}"

sleep 0.3

cat > "${NODES_ROOT}/info.txt" <<EOF
Hyperliquid Mock Harness
  API endpoint: ${API_URL}
  WS endpoint:  ws://${ADDR}/ws

  Point a HYPE wallet at the mock via wallet settings:
    apiendpoint = ${API_URL}
    wsendpoint  = ws://${ADDR}/ws
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

# Helper: credit an address with HYPE via the mock's /test/fund endpoint.
# Unlike the testnet harness's "send", this mints balance directly rather
# than transferring from a funded account — the mock has no faucet.
cat > "${NODES_ROOT}/send" <<SENDEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ] || [ -z "\$2" ]; then
  echo "Usage: ./send <0x_recipient> <amount_hype>"
  exit 1
fi
ATOMS=\$(python3 -c "import sys; print(int(round(float(sys.argv[1]) * 1e8)))" "\$2")
curl -sf -X POST "${API_URL}/test/fund" \
  -H "Content-Type: application/json" \
  -d "{\"address\":\"\$1\",\"amountAtoms\":\${ATOMS}}" -o /dev/null
echo "Credited \$2 HYPE to \$1"
SENDEOF
chmod +x "${NODES_ROOT}/send"

cat <<EOF

Hyperliquid mock server running (pid $(cat "${PIDFILE}")), log: ${NODES_ROOT}/mockserver.log

Wallet config:
  apiendpoint = ${API_URL}
  wsendpoint  = ws://${ADDR}/ws

Helpers:
  ${NODES_ROOT}/send <address> <amount_HYPE>
  ${NODES_ROOT}/balance <address>

Stop with: ./harness.sh stop
EOF
