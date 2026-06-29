#!/usr/bin/env bash
# Solana simnet harness backed by solana-test-validator.
#
# Usage:
#   ./harness.sh        # start the harness
#   ./harness.sh stop   # stop the harness

set -e

SESSION="sol-harness"
NODES_ROOT=~/dextest/sol
LEDGER_DIR="${NODES_ROOT}/ledger"
HARNESS_DIR="${NODES_ROOT}/harness-ctl"
RPC_PORT="18999"
WS_PORT="19000"
GOSSIP_PORT="19001"
DOCKER_IMAGE="ghcr.io/beeman/solana-test-validator:latest"
DOCKER_CONTAINER="sol-simnet"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
USE_DOCKER=0
WAIT_SECONDS=180

if [ "$1" = "stop" ]; then
  tmux kill-session -t "${SESSION}" 2>/dev/null || true
  docker stop "${DOCKER_CONTAINER}" 2>/dev/null || true
  docker rm "${DOCKER_CONTAINER}" 2>/dev/null || true
  echo "stopped."
  exit 0
fi

if command -v solana-test-validator >/dev/null 2>&1; then
  VALIDATOR_BIN="solana-test-validator"
elif command -v docker >/dev/null 2>&1; then
  USE_DOCKER=1
else
  echo "Neither solana-test-validator nor docker is available." >&2
  exit 1
fi

tmux kill-session -t "${SESSION}" 2>/dev/null || true
docker stop "${DOCKER_CONTAINER}" 2>/dev/null || true
docker rm "${DOCKER_CONTAINER}" 2>/dev/null || true
rm -rf "${NODES_ROOT}"
mkdir -p "${LEDGER_DIR}" "${HARNESS_DIR}"

if [ "${USE_DOCKER}" = "1" ]; then
  docker pull "${DOCKER_IMAGE}"
fi

cat > "${HARNESS_DIR}/quit" <<EOF
#!/usr/bin/env bash
tmux kill-session -t "${SESSION}" 2>/dev/null || true
docker stop "${DOCKER_CONTAINER}" 2>/dev/null || true
docker rm "${DOCKER_CONTAINER}" 2>/dev/null || true
EOF
chmod +x "${HARNESS_DIR}/quit"

cat > "${HARNESS_DIR}/slot" <<EOF
#!/usr/bin/env bash
curl -sf http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"confirmed"}]
}' | python3 -m json.tool
EOF
chmod +x "${HARNESS_DIR}/slot"

cat > "${HARNESS_DIR}/balance" <<EOF
#!/usr/bin/env bash
if [ -z "\$1" ]; then
  echo "Usage: ./balance <address>"
  exit 1
fi
curl -sf http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json' -d "{
  \"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getBalance\",\"params\":[\"\$1\",{\"commitment\":\"confirmed\"}]
}" | python3 -m json.tool
EOF
chmod +x "${HARNESS_DIR}/balance"

cat > "${HARNESS_DIR}/airdrop" <<EOF
#!/usr/bin/env bash
if [ -z "\$1" ] || [ -z "\$2" ]; then
  echo "Usage: ./airdrop <address> <sol>"
  exit 1
fi
LAMPORTS=\$(python3 - <<'PY' "\$2"
import sys
print(int(float(sys.argv[1]) * 1_000_000_000))
PY
)
curl -sf http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json' -d "{
  \"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"requestAirdrop\",\"params\":[\"\$1\",\${LAMPORTS},{\"commitment\":\"confirmed\"}]
}" | python3 -m json.tool
EOF
chmod +x "${HARNESS_DIR}/airdrop"

cat > "${HARNESS_DIR}/confirm" <<EOF
#!/usr/bin/env bash
if [ -z "\$1" ]; then
  echo "Usage: ./confirm <signature>"
  exit 1
fi
curl -sf http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json' -d "{
  \"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getSignatureStatuses\",\"params\":[[\"\$1\"],{\"searchTransactionHistory\":true}]
}" | python3 -m json.tool
EOF
chmod +x "${HARNESS_DIR}/confirm"

# faucet_addr extracts the base58 address from a Solana keypair JSON file.
faucet_addr() {
  python3 - "${LEDGER_DIR}/faucet-keypair.json" <<'PY'
import json, sys
ALPHABET = b'123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'
def b58(data):
    n = int.from_bytes(data, 'big')
    r = []
    while n:
        n, i = divmod(n, 58)
        r.append(ALPHABET[i:i+1])
    r.reverse()
    return b''.join(r).decode()
kp = json.load(open(sys.argv[1]))
print(b58(bytes(kp[32:])))
PY
}

FAUCET_KEYPAIR_PATH="${LEDGER_DIR}/faucet-keypair.json"

if [ "${USE_DOCKER}" = "1" ]; then
  SEND_CMD="docker exec ${DOCKER_CONTAINER} /workspace/bin/solana transfer --from /ledger/faucet-keypair.json --keypair /ledger/faucet-keypair.json"
  SEND_URL="http://127.0.0.1:8899"
else
  SEND_CMD="solana transfer --from ${LEDGER_DIR}/faucet-keypair.json --keypair ${LEDGER_DIR}/faucet-keypair.json"
  SEND_URL="http://127.0.0.1:${RPC_PORT}"
fi

cat > "${HARNESS_DIR}/send" <<EOF
#!/usr/bin/env bash
if [ -z "\$1" ] || [ -z "\$2" ]; then
  echo "Usage: ./send <recipient_address> <sol>"
  exit 1
fi
${SEND_CMD} "\$1" "\$2" --url ${SEND_URL} --allow-unfunded-recipient
EOF
chmod +x "${HARNESS_DIR}/send"

cat > "${HARNESS_DIR}/faucet" <<EOF
#!/usr/bin/env bash
ADDR=\$(python3 - "${FAUCET_KEYPAIR_PATH}" <<'PY'
import json, sys
ALPHABET = b'123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'
def b58(data):
    n = int.from_bytes(data, 'big')
    r = []
    while n:
        n, i = divmod(n, 58)
        r.append(ALPHABET[i:i+1])
    r.reverse()
    return b''.join(r).decode()
kp = json.load(open(sys.argv[1]))
print(b58(bytes(kp[32:])))
PY
)
echo "Faucet address: \${ADDR}"
curl -sf http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json' -d "{
  \"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getBalance\",\"params\":[\"\${ADDR}\",{\"commitment\":\"confirmed\"}]
}" | python3 -m json.tool
EOF
chmod +x "${HARNESS_DIR}/faucet"

cat > "${NODES_ROOT}/rpc_endpoint.txt" <<EOF
http://127.0.0.1:${RPC_PORT}
EOF

cat > "${NODES_ROOT}/ws_endpoint.txt" <<EOF
ws://127.0.0.1:${WS_PORT}
EOF

cat > "${NODES_ROOT}/info.txt" <<EOF
Solana simnet harness
  RPC endpoint: http://127.0.0.1:${RPC_PORT}
  WS endpoint:  ws://127.0.0.1:${WS_PORT}
  Data dir:     ${NODES_ROOT}
  Mode:         $( [ "${USE_DOCKER}" = "1" ] && echo docker || echo local )
EOF

tmux new-session -d -s "${SESSION}" "${SHELL}"
tmux rename-window -t "${SESSION}:0" "validator"
tmux send-keys -t "${SESSION}:0" "cd ${NODES_ROOT}" C-m
if [ "${USE_DOCKER}" = "1" ]; then
  docker run -d \
    --name "${DOCKER_CONTAINER}" \
    --security-opt seccomp=unconfined \
    -p "${RPC_PORT}:8899" \
    -p "${WS_PORT}:8900" \
    -v "${LEDGER_DIR}:/ledger" \
    "${DOCKER_IMAGE}" \
    solana-test-validator \
    --log \
    --reset \
    --ledger /ledger \
    --limit-ledger-size 200000000 >/dev/null
  tmux send-keys -t "${SESSION}:0" "docker logs -f ${DOCKER_CONTAINER}" C-m
else
  tmux send-keys -t "${SESSION}:0" "${VALIDATOR_BIN} --log --reset --ledger ${LEDGER_DIR} --rpc-port ${RPC_PORT} --limit-ledger-size 200000000" C-m
fi

tmux new-window -t "${SESSION}:1" -n "harness-ctl"
tmux send-keys -t "${SESSION}:1" "cd ${HARNESS_DIR}" C-m

ready_count=0
for i in $(seq 1 "${WAIT_SECONDS}"); do
  if curl -sf http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"getSlot"}' >/dev/null 2>&1; then
    ready_count=$((ready_count + 1))
  else
    ready_count=0
  fi
  if [ "${ready_count}" -ge 3 ]; then
    echo ""
    echo "Solana simnet harness is running."
    echo "  RPC:     http://127.0.0.1:${RPC_PORT}"
    echo "  WS:      ws://127.0.0.1:${WS_PORT}"
    echo "  Data:    ${NODES_ROOT}"
    echo "  Control: ${HARNESS_DIR}"
    echo ""
    echo "  Helpers:"
    echo "    ${HARNESS_DIR}/send <recipient> <sol>   # send from faucet"
    echo "    ${HARNESS_DIR}/airdrop <addr> <sol>     # mint new SOL"
    echo "    ${HARNESS_DIR}/faucet                   # faucet address + balance"
    echo "    ${HARNESS_DIR}/balance <addr>           # check balance"
    echo "    ${HARNESS_DIR}/confirm <sig>            # check tx status"
    echo ""
    tmux attach -t "${SESSION}"
    exit 0
  fi
  sleep 1
done

echo "ERROR: Solana validator did not start within ${WAIT_SECONDS} seconds." >&2
tmux capture-pane -t "${SESSION}:0" -p >&2 || true
exit 1
