#!/usr/bin/env bash
# Tmux script that sets up the simnet NEAR Intents 1Click mock harness,
# backed by the local chain harnesses. The chain harnesses for every enabled
# slug must already be running (e.g. util/testing/btc/harness.sh and
# util/testing/near/harness.sh for the default btc,near set).
#
# Usage:
#   ./harness.sh          # build and start the tmux session
#   ./harness.sh stop     # stop the mock and kill the session
#
# Environment:
#   CHAINS   comma-separated chain slugs to enable, or "auto" (default) to
#            enable every chain whose harness is running
#   RATES    caribou app server base URL for live USD rates
#            (default http://127.0.0.3:5758; set empty to disable)
#
# A wallet started with --simnet picks the mock up automatically (the
# baked-in simnet defaults point at this address and signing key); the
# --intentsapi and --intentssigningkey options override.
#
# The session is named near-intents-harness: window 0 (intentsmock) runs the
# mock in the foreground, window 1 (harness-ctl) holds the helper scripts in
# ~/dextest/near-intents/harness-ctl/:
#   ./status <depositAddress|last>       # query a swap's 1Click status
#   ./swaps                              # dump all swap states
#   ./lastswap                           # deposit address of the newest swap
#   ./force <outcome>                    # arm REFUND|FAIL|STALL for the NEXT swap
#   ./force last <outcome>               # force the newest swap
#   ./force <depositAddress> <outcome>   # force a specific swap
#   ./quit                               # shut down the mock and the session
set -e

NODES_ROOT=~/dextest/near-intents
HARNESS_DIR="${NODES_ROOT}/harness-ctl"
ADDR="127.0.0.1:17781"
API_URL="http://${ADDR}"
CHAINS="${CHAINS:-auto}"
RATES="${RATES-http://127.0.0.3:5758}"
SESSION="near-intents-harness"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# stop_running takes down the tmux session and any stray mock process — one
# orphaned from a killed session, or left by the old daemonized version of
# this script. Returns non-zero if nothing was running.
stop_running() {
  local stopped=1
  if tmux has-session -t "${SESSION}" 2>/dev/null; then
    tmux kill-session -t "${SESSION}"
    stopped=0
  fi
  if pkill -f "intentsmock -addr ${ADDR}" 2>/dev/null; then
    stopped=0
  fi
  return "${stopped}"
}

if [ "$1" = "stop" ]; then
  if stop_running; then
    echo "Stopped intents mock harness."
  else
    echo "No running intents mock harness."
  fi
  exit 0
fi

# A restart must take the old instance down first: the new one cannot bind
# the port while the old one lives.
if stop_running; then
  echo "Stopped the running intents mock harness."
  sleep 0.3
fi

rm -rf "${NODES_ROOT}"
mkdir -p "${HARNESS_DIR}"

echo "Building intentsmock..."
go build -C "${REPO_ROOT}" -o "${NODES_ROOT}/intentsmock" ./util/testing/near-intents/cmd/intentsmock/

PUBKEY="$("${NODES_ROOT}/intentsmock" -pubkey)"

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

cat > "${HARNESS_DIR}/lastswap" <<LASTEOF
#!/usr/bin/env bash
# Prints the deposit address of the most recently created swap.
set -e
curl -sf "${API_URL}/test/swaps" | python3 -c '
import json, sys
swaps = json.load(sys.stdin)
if not swaps:
    sys.exit("no swaps yet")
swaps.sort(key=lambda s: s["createdAt"])
print(swaps[-1]["depositAddress"])'
LASTEOF
chmod +x "${HARNESS_DIR}/lastswap"

cat > "${HARNESS_DIR}/status" <<STATUSEOF
#!/usr/bin/env bash
set -e
if [ -z "\$1" ]; then
  echo "Usage: ./status <depositAddress|last>"
  exit 1
fi
ADDR="\$1"
if [ "\$ADDR" = "last" ]; then
  ADDR="\$("\$(dirname "\$0")/lastswap")"
fi
curl -sf "${API_URL}/v0/status?depositAddress=\${ADDR}" | python3 -m json.tool
STATUSEOF
chmod +x "${HARNESS_DIR}/status"

cat > "${HARNESS_DIR}/swaps" <<SWAPSEOF
#!/usr/bin/env bash
set -e
curl -sf "${API_URL}/test/swaps" | python3 -m json.tool
SWAPSEOF
chmod +x "${HARNESS_DIR}/swaps"

# Note: a swap held in STALL past its quote deadline plus the wallet's
# 10-minute grace is failed locally by the wallet even if later resumed.
cat > "${HARNESS_DIR}/force" <<FORCEEOF
#!/usr/bin/env bash
set -e
usage() {
  echo "Usage: ./force <REFUND|FAIL|STALL|RESUME>          arm the NEXT swap created"
  echo "       ./force last <outcome>                      most recently created swap"
  echo "       ./force <depositAddress> <outcome>"
  echo
  echo "Arming before executing in the UI is race-free: the swap is created"
  echo "with the outcome already set. RESUME with no address clears the arm."
  exit 1
}
case "\$1" in
REFUND|FAIL|STALL|RESUME)
  [ -n "\$2" ] && usage
  ADDR=""
  OUTCOME="\$1"
  ;;
last)
  [ -z "\$2" ] && usage
  ADDR="\$("\$(dirname "\$0")/lastswap")"
  OUTCOME="\$2"
  ;;
*)
  if [ -z "\$1" ] || [ -z "\$2" ]; then usage; fi
  ADDR="\$1"
  OUTCOME="\$2"
  ;;
esac
curl -sf -X POST "${API_URL}/test/force" \
  -H "Content-Type: application/json" \
  -d "{\"depositAddress\":\"\${ADDR}\",\"outcome\":\"\${OUTCOME}\"}"
echo
FORCEEOF
chmod +x "${HARNESS_DIR}/force"

cat > "${HARNESS_DIR}/quit" <<EOF
#!/usr/bin/env bash
tmux send-keys -t ${SESSION}:0 C-c
tmux wait-for nearintents
# seppuku
tmux kill-session -t ${SESSION}
EOF
chmod +x "${HARNESS_DIR}/quit"

cd "${NODES_ROOT}" && tmux new-session -d -s "${SESSION}" "$SHELL"

tmux rename-window -t "${SESSION}:0" 'intentsmock'
tmux send-keys -t "${SESSION}:0" "set +o history" C-m
tmux send-keys -t "${SESSION}:0" "cd ${NODES_ROOT}" C-m
tmux send-keys -t "${SESSION}:0" "./intentsmock -addr \"${ADDR}\" -chains \"${CHAINS}\" -rates \"${RATES}\" 2>&1 | tee intentsmock.log; tmux wait-for -S nearintents" C-m

tmux new-window -t "${SESSION}:1" -n 'harness-ctl' "$SHELL"
tmux send-keys -t "${SESSION}:1" "set +o history" C-m
tmux send-keys -t "${SESSION}:1" "cd ${HARNESS_DIR}" C-m

# Wait for the mock to come up before declaring success.
up=""
for _ in $(seq 1 20); do
  if curl -sf "${API_URL}/test/swaps" > /dev/null 2>&1; then
    up=1
    break
  fi
  sleep 0.25
done
if [ -z "${up}" ]; then
  echo "intentsmock failed to start:"
  tail -20 "${NODES_ROOT}/intentsmock.log" 2>/dev/null || true
  tmux kill-session -t "${SESSION}" 2>/dev/null || true
  exit 1
fi

cat <<EOF

Intents mock harness running in tmux session ${SESSION}
(windows: 0 intentsmock, 1 harness-ctl), log: ${NODES_ROOT}/intentsmock.log

  API endpoint: ${API_URL}
  Signing key:  ${PUBKEY}
  Chains:       ${CHAINS}

Attach with:  tmux attach-session -t ${SESSION}
Stop with:    ${HARNESS_DIR}/quit   (or ./harness.sh stop)
EOF
