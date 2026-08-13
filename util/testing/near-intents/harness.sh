#!/usr/bin/env bash
# Starts the simnet NEAR Intents 1Click mock server, backed by the local
# chain harnesses. The chain harnesses for every enabled slug must already
# be running (e.g. util/testing/btc/harness.sh and
# util/testing/near/harness.sh for the default btc,near set).
#
# Usage:
#   ./harness.sh          # build and start
#   ./harness.sh stop     # stop the running mock server
#
# Environment:
#   CHAINS   comma-separated chain slugs to enable (default btc,near)
#   RATES    caribou app server base URL for live USD rates
#            (default http://127.0.0.3:5758; set empty to disable)
#
# A wallet started with --simnet picks the mock up automatically (the
# baked-in simnet defaults point at this address and signing key); the
# --intentsapi and --intentssigningkey options override.
#
# After setup, helpers are available in ~/dextest/near-intents/:
#   ./status <depositAddress>            # query a swap's 1Click status
#   ./swaps                              # dump all swap states
#   ./force <depositAddress> <outcome>   # force REFUND|FAIL|STALL|RESUME
set -e

NODES_ROOT=~/dextest/near-intents
ADDR="127.0.0.1:17781"
API_URL="http://${ADDR}"
PIDFILE="${NODES_ROOT}/intentsmock.pid"
CHAINS="${CHAINS:-btc,near}"
RATES="${RATES-http://127.0.0.3:5758}"

HARNESS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HARNESS_DIR}/../../.." && pwd)"

if [ "$1" = "stop" ]; then
  if [ -f "${PIDFILE}" ]; then
    kill "$(cat "${PIDFILE}")" 2>/dev/null || true
    rm -f "${PIDFILE}"
    echo "Stopped intents mock server."
  else
    echo "No running intents mock server (no pidfile at ${PIDFILE})."
  fi
  exit 0
fi

mkdir -p "${NODES_ROOT}"

echo "Building intentsmock..."
go build -C "${REPO_ROOT}" -o "${NODES_ROOT}/intentsmock" ./util/testing/near-intents/cmd/intentsmock/

PUBKEY="$("${NODES_ROOT}/intentsmock" -pubkey)"

"${NODES_ROOT}/intentsmock" -addr "${ADDR}" -chains "${CHAINS}" -rates "${RATES}" \
  > "${NODES_ROOT}/intentsmock.log" 2>&1 &
echo $! > "${PIDFILE}"

sleep 0.3
if ! kill -0 "$(cat "${PIDFILE}")" 2>/dev/null; then
  echo "intentsmock failed to start:"
  cat "${NODES_ROOT}/intentsmock.log"
  rm -f "${PIDFILE}"
  exit 1
fi

cat > "${NODES_ROOT}/info.txt" <<EOF
NEAR Intents 1Click Mock Harness
  API endpoint: ${API_URL}
  Signing key:  ${PUBKEY}
  Chains:       ${CHAINS}

  A --simnet wallet uses this endpoint and key by default. Explicit
  overrides, or use on another network:
    --intentsapi=${API_URL}
    --intentssigningkey=${PUBKEY}
EOF

cat > "${NODES_ROOT}/status" <<STATUSEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ]; then
  echo "Usage: ./status <depositAddress>"
  exit 1
fi
curl -sf "${API_URL}/v0/status?depositAddress=\$1" | python3 -m json.tool
STATUSEOF
chmod +x "${NODES_ROOT}/status"

cat > "${NODES_ROOT}/swaps" <<SWAPSEOF
#!/usr/bin/env bash
set -e
curl -sf "${API_URL}/test/swaps" | python3 -m json.tool
SWAPSEOF
chmod +x "${NODES_ROOT}/swaps"

# Note: a swap held in STALL past its quote deadline plus the wallet's
# 10-minute grace is failed locally by the wallet even if later resumed.
cat > "${NODES_ROOT}/force" <<FORCEEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ] || [ -z "\$2" ]; then
  echo "Usage: ./force <depositAddress> <REFUND|FAIL|STALL|RESUME>"
  exit 1
fi
curl -sf -X POST "${API_URL}/test/force" \
  -H "Content-Type: application/json" \
  -d "{\"depositAddress\":\"\$1\",\"outcome\":\"\$2\"}"
echo
FORCEEOF
chmod +x "${NODES_ROOT}/force"

cat <<EOF

Intents mock server running (pid $(cat "${PIDFILE}")), log: ${NODES_ROOT}/intentsmock.log

  API endpoint: ${API_URL}
  Signing key:  ${PUBKEY}
  Chains:       ${CHAINS}

Helpers:
  ${NODES_ROOT}/status <depositAddress>
  ${NODES_ROOT}/swaps
  ${NODES_ROOT}/force <depositAddress> <REFUND|FAIL|STALL|RESUME>

Stop with: ./harness.sh stop
EOF
