#!/usr/bin/env bash
# XRP Ledger simnet harness.
# Starts a rippled node in standalone mode inside a tmux session.
# Ledgers are advanced by a background ticker (window 1) and on demand
# via harness-ctl/mine.
#
# Usage:
#   ./harness.sh        # start the harness
#   ./harness.sh stop   # kill the session and clean up
#
# Prerequisites: rippled or xrpld on PATH, OR Docker (image built automatically).
#   rippled/xrpld: https://xrpl.org/install-rippled.html
#   XRP_RIPPLED=/path/to/binary for a source build not on PATH
#   Docker:  https://docs.docker.com/get-docker/

set -e

# cleanup tears the harness down only if setup did NOT complete (e.g. an error
# under `set -e`, or Ctrl-C mid-setup). Once HARNESS_STARTED=1, the harness is
# fully up and we've attached to tmux; detaching from that session returns
# normally and must leave the harness running, so cleanup must do nothing.
cleanup() {
    local rc=$?
    if [[ "${HARNESS_STARTED:-0}" != "1" ]]; then
        echo "Harness setup interrupted (rc=$rc); cleaning up..."
        tmux kill-session -t "${SESSION}" 2>/dev/null || true
        docker stop xrp-simnet 2>/dev/null || true
        docker rm xrp-simnet 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

SYMBOL="xrp"
SESSION="xrp-harness"
RPC_PORT="15005"
WS_PORT="16006"
PEER_PORT="51235"
NODES_ROOT=~/dextest/xrp
ALPHA_DIR="${NODES_ROOT}/alpha"
HARNESS_DIR="${NODES_ROOT}/harness-ctl"
DOCKER_CFG_DIR="${NODES_ROOT}/docker-config"
DOCKER_IMAGE="xrp-harness"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

################################################################################
# Stop
################################################################################

if [ "$1" = "stop" ]; then
  tmux kill-session -t "${SESSION}" 2>/dev/null || true
  docker stop xrp-simnet 2>/dev/null || true
  echo "stopped."
  exit 0
fi

################################################################################
# Preflight: detect rippled or Docker
################################################################################

# The server binary was renamed from "rippled" to "xrpld" in the develop tree
# (released apt packages still ship "rippled"). Accept either name, and allow an
# explicit override via XRP_RIPPLED for a source build that isn't on PATH, e.g.
#   XRP_RIPPLED=/path/to/rippled/build/xrpld ./harness.sh
RIPPLED_BIN=""
if [ -n "${XRP_RIPPLED:-}" ]; then
  if ! command -v "${XRP_RIPPLED}" > /dev/null 2>&1; then
    echo "XRP_RIPPLED is set to '${XRP_RIPPLED}' but it is not executable." >&2
    exit 1
  fi
  RIPPLED_BIN="${XRP_RIPPLED}"
else
  for candidate in rippled xrpld; do
    if command -v "${candidate}" > /dev/null 2>&1; then
      RIPPLED_BIN="${candidate}"
      break
    fi
  done
fi

if [ -n "${RIPPLED_BIN}" ]; then
  USE_DOCKER=0
elif command -v docker > /dev/null 2>&1; then
  USE_DOCKER=1
else
  echo "Neither rippled nor xrpld found on PATH, and docker is not available." >&2
  echo "  Set XRP_RIPPLED=/path/to/binary to use a source build off PATH." >&2
  echo "  rippled: https://xrpl.org/install-rippled.html" >&2
  echo "  Docker:  https://docs.docker.com/get-docker/" >&2
  exit 1
fi

################################################################################
# Preflight: version check (local rippled only)
################################################################################

if [ "${USE_DOCKER}" = "0" ] && [ -z "${XRP_HARNESS_SKIP_VERSION_CHECK}" ]; then
  LOCAL_VERSION=$("${RIPPLED_BIN}" --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
  if [ -n "${LOCAL_VERSION}" ]; then
    LATEST=$(curl --max-time 3 -sf \
      "https://api.github.com/repos/XRPLF/rippled/releases/latest" \
      2>/dev/null | grep '"tag_name"' | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || echo "")
    if [ -n "${LATEST}" ]; then
      LOCAL_MINOR=$(echo "${LOCAL_VERSION}" | cut -d. -f1-2)
      LATEST_MINOR=$(echo "${LATEST}" | cut -d. -f1-2)
      if [ "${LOCAL_MINOR}" != "${LATEST_MINOR}" ]; then
        echo "================================================================" >&2
        echo "WARNING: ${RIPPLED_BIN} is out of date." >&2
        echo "  Installed: ${LOCAL_VERSION}" >&2
        echo "  Latest:    ${LATEST}" >&2
        echo "  Update:    https://xrpl.org/install-rippled.html" >&2
        echo "================================================================" >&2
        if [ -t 0 ]; then
          read -r -p "Continue with this ${RIPPLED_BIN} version? [Y/n] " REPLY
          case "${REPLY}" in
            n|N) exit 1 ;;
          esac
        fi
      fi
    fi
  fi
fi

################################################################################
# Clean up previous run
################################################################################

tmux kill-session -t "${SESSION}" 2>/dev/null || true
docker stop xrp-simnet 2>/dev/null || true
rm -rf "${NODES_ROOT}"
mkdir -p "${ALPHA_DIR}" "${HARNESS_DIR}"

################################################################################
# Build sendxrp
################################################################################

echo "Building sendxrp..."
go build -C "${REPO_ROOT}" -o "${NODES_ROOT}/sendxrp" \
  ./util/testing/xrp/sendxrp/

if [ "${USE_DOCKER}" = "1" ] && ! docker image inspect "${DOCKER_IMAGE}" > /dev/null 2>&1; then
  echo "Building Docker image ${DOCKER_IMAGE}..."
  docker build --platform linux/amd64 -t "${DOCKER_IMAGE}" "${SCRIPT_DIR}"
fi

################################################################################
# Write rippled config
################################################################################

if [ "${USE_DOCKER}" = "0" ]; then
  cat > "${ALPHA_DIR}/rippled.cfg" <<EOF
[server]
port_rpc_admin_local
port_ws_admin_local
port_peer

[port_rpc_admin_local]
port = ${RPC_PORT}
ip = 127.0.0.1
admin = 127.0.0.1
protocol = http

[port_ws_admin_local]
port = ${WS_PORT}
ip = 127.0.0.1
admin = 127.0.0.1
protocol = ws

[port_peer]
port = ${PEER_PORT}
ip = 127.0.0.1
protocol = peer

[node_size]
tiny

[node_db]
type=NuDB
path=${ALPHA_DIR}/db/nudb
online_delete=256
advisory_delete=0

[database_path]
${ALPHA_DIR}/db

[debug_logfile]
${ALPHA_DIR}/debug.log

[ips_fixed]

[validators_file]
${ALPHA_DIR}/validators.txt

[rpc_startup]
{ "command": "log_level", "severity": "info" }

[ledger_history]
full

[fetch_depth]
full

[voting]
account_reserve=1000000
owner_reserve=200000
EOF
  cp "${SCRIPT_DIR}/validators.txt" "${ALPHA_DIR}/validators.txt"
else
  mkdir -p "${DOCKER_CFG_DIR}"
  cat > "${DOCKER_CFG_DIR}/rippled.cfg" <<'EOF'
[server]
port_rpc_admin_local
port_ws_admin_local

[port_rpc_admin_local]
port = 5005
ip = 0.0.0.0
admin = 0.0.0.0
protocol = http

[port_ws_admin_local]
port = 6006
ip = 0.0.0.0
admin = 0.0.0.0
protocol = ws

[node_size]
tiny

[node_db]
type=NuDB
path=/var/lib/rippled/db/nudb
online_delete=256
advisory_delete=0

[database_path]
/var/lib/rippled/db

[debug_logfile]
/var/log/rippled/debug.log

[ips_fixed]

[validators_file]
/etc/opt/ripple/validators.txt

[rpc_startup]
{ "command": "log_level", "severity": "info" }

[ledger_history]
full

[fetch_depth]
full

[voting]
account_reserve=1000000
owner_reserve=200000
EOF
  cp "${SCRIPT_DIR}/validators.txt" "${DOCKER_CFG_DIR}/validators.txt"
fi

################################################################################
# Write harness-ctl scripts
################################################################################

RPC_CLI="curl -sf -X POST http://127.0.0.1:${RPC_PORT} -H 'Content-Type: application/json'"

cat > "${HARNESS_DIR}/rippled-cli" <<EOF
#!/usr/bin/env bash
# Generic admin RPC wrapper. Usage: ./rippled-cli <method> [json-params]
METHOD="\$1"
PARAMS="\${2:-{}}"
${RPC_CLI} -d "{\"method\":\"\${METHOD}\",\"params\":[\${PARAMS}]}"
EOF

cat > "${HARNESS_DIR}/mine" <<EOF
#!/usr/bin/env bash
# Advance the ledger N times (default 1).
N="\${1:-1}"
for i in \$(seq 1 "\$N"); do
  ${RPC_CLI} -d '{"method":"ledger_accept","params":[{}]}' > /dev/null
done
echo "advanced \$N ledger(s)"
EOF

cat > "${HARNESS_DIR}/tick-loop" <<EOF
#!/usr/bin/env bash
# Background ticker: closes a ledger every 2 seconds.
while true; do
  ${RPC_CLI} -d '{"method":"ledger_accept","params":[{}]}' > /dev/null 2>&1
  sleep 2
done
EOF

cat > "${HARNESS_DIR}/fund" <<EOF
#!/usr/bin/env bash
# Fund an address from the genesis account. Usage: ./fund <address> <xrp>
if [ \$# -ne 2 ]; then echo "usage: fund <address> <xrp>"; exit 1; fi
DROPS=\$(awk "BEGIN{printf \"%.0f\", \$2 * 1000000}")
${NODES_ROOT}/sendxrp "http://127.0.0.1:${RPC_PORT}" "\$1" "\${DROPS}"
EOF

cat > "${HARNESS_DIR}/balance" <<EOF
#!/usr/bin/env bash
# Print account balance. Usage: ./balance <address>
${RPC_CLI} -d "{\"method\":\"account_info\",\"params\":[{\"account\":\"\$1\",\"ledger_index\":\"validated\"}]}"
EOF

cat > "${HARNESS_DIR}/tip" <<'TIPEOF'
#!/usr/bin/env bash
# Print latest validated and current ledger indices.
RAW=$(curl -sf -X POST http://127.0.0.1:15005 -H 'Content-Type: application/json' \
  -d '{"method":"server_info","params":[{}]}')
VALIDATED=$(printf '%s' "$RAW" | grep -o '"seq":[0-9]*' | head -1 | grep -o '[0-9]*')
# In standalone mode ledger_index is absent; derive current from complete_ledgers range end.
CURRENT=$(printf '%s' "$RAW" | grep -o '"complete_ledgers":"[^"]*"' | grep -o '[0-9]*"$' | tr -d '"')
echo "validated: ${VALIDATED:-?}"
echo "current:   ${CURRENT:-?}"
TIPEOF

cat > "${HARNESS_DIR}/server" <<EOF
#!/usr/bin/env bash
# Print server_state (reserves, fees, etc.)
${RPC_CLI} -d '{"method":"server_state","params":[{}]}'
EOF

cat > "${HARNESS_DIR}/submit" <<EOF
#!/usr/bin/env bash
# Submit a pre-signed tx blob. Usage: ./submit <hex-blob>
${RPC_CLI} -d "{\"method\":\"submit\",\"params\":[{\"tx_blob\":\"\$1\"}]}"
EOF

cat > "${HARNESS_DIR}/tx" <<EOF
#!/usr/bin/env bash
# Look up a transaction by hash. Usage: ./tx <hash>
${RPC_CLI} -d "{\"method\":\"tx\",\"params\":[{\"transaction\":\"\$1\"}]}"
EOF

# Issued-currency (IOU) helpers. The genesis account doubles as the issuer:
# any funded account can issue an IOU, and the wallet's simnet usdc.xrp token
# names genesis as its issuer (wallet/asset/xrp/params.go). Signing is done
# server-side by rippled from the seed, which the admin RPC permits.
USDC_HEX="5553444300000000000000000000000000000000"
GENESIS_ADDR="rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
GENESIS_SEED="snoPBrXtMeMyMHUVTgbuqAfg1SUTb"

cat > "${HARNESS_DIR}/issue" <<EOF
#!/usr/bin/env bash
# Pay an issued currency out of the genesis account.
# Usage: ./issue <address> <amount> [currency]
# currency defaults to USDC's 160-bit hex code, the code the wallet's usdc.xrp
# token uses; a 3-letter code or another 40-hex code may be given instead. The
# destination must already hold a trust line to genesis for the currency (the
# wallet establishes its own; use ./trustset for a harness account).
if [ \$# -lt 2 ]; then echo "usage: issue <address> <amount> [currency]"; exit 1; fi
CURRENCY="\${3:-${USDC_HEX}}"
RAW=\$(${RPC_CLI} -d "{\"method\":\"submit\",\"params\":[{\"secret\":\"${GENESIS_SEED}\",\"tx_json\":{\"TransactionType\":\"Payment\",\"Account\":\"${GENESIS_ADDR}\",\"Destination\":\"\$1\",\"Amount\":{\"currency\":\"\${CURRENCY}\",\"issuer\":\"${GENESIS_ADDR}\",\"value\":\"\$2\"}}}]}")
RESULT=\$(printf '%s' "\$RAW" | grep -o '"engine_result":"[^"]*"' | head -1 | cut -d'"' -f4)
HASH=\$(printf '%s' "\$RAW" | grep -o '"hash":"[A-F0-9]*"' | head -1 | cut -d'"' -f4)
if [ -z "\$RESULT" ]; then printf '%s\n' "\$RAW"; exit 1; fi
echo "issued \$2 \${CURRENCY} to \$1: \${RESULT} \${HASH}"
EOF

cat > "${HARNESS_DIR}/newaccount" <<EOF
#!/usr/bin/env bash
# Create a funded account with a known seed, for use as a counterparty.
# Usage: ./newaccount [xrp]   (default 20 XRP)
# Prints the address and seed; the seed is what ./trustset takes.
XRP="\${1:-20}"
RAW=\$(${RPC_CLI} -d '{"method":"wallet_propose","params":[{"key_type":"secp256k1"}]}')
ADDR=\$(printf '%s' "\$RAW" | grep -o '"account_id":"[^"]*"' | head -1 | cut -d'"' -f4)
SEED=\$(printf '%s' "\$RAW" | grep -o '"master_seed":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -z "\$ADDR" ]; then printf '%s\n' "\$RAW"; exit 1; fi
"${HARNESS_DIR}/fund" "\$ADDR" "\$XRP" > /dev/null
"${HARNESS_DIR}/mine" > /dev/null
echo "address: \$ADDR"
echo "seed:    \$SEED"
EOF

cat > "${HARNESS_DIR}/trustset" <<EOF
#!/usr/bin/env bash
# Open a trust line from a harness account to the genesis issuer, so ./issue
# can pay it and the wallet can send to it.
# Usage: ./trustset <seed> [currency] [limit]
# currency defaults to USDC's hex code; limit defaults to 1000000000.
if [ \$# -lt 1 ]; then echo "usage: trustset <seed> [currency] [limit]"; exit 1; fi
SEED="\$1"
CURRENCY="\${2:-${USDC_HEX}}"
LIMIT="\${3:-1000000000}"
RAW=\$(${RPC_CLI} -d "{\"method\":\"wallet_propose\",\"params\":[{\"seed\":\"\${SEED}\",\"key_type\":\"secp256k1\"}]}")
ADDR=\$(printf '%s' "\$RAW" | grep -o '"account_id":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -z "\$ADDR" ]; then printf '%s\n' "\$RAW"; exit 1; fi
# Flags 131072 = tfSetNoRipple, matching the wallet's own TrustSet.
RAW=\$(${RPC_CLI} -d "{\"method\":\"submit\",\"params\":[{\"secret\":\"\${SEED}\",\"tx_json\":{\"TransactionType\":\"TrustSet\",\"Account\":\"\${ADDR}\",\"Flags\":131072,\"LimitAmount\":{\"currency\":\"\${CURRENCY}\",\"issuer\":\"${GENESIS_ADDR}\",\"value\":\"\${LIMIT}\"}}}]}")
RESULT=\$(printf '%s' "\$RAW" | grep -o '"engine_result":"[^"]*"' | head -1 | cut -d'"' -f4)
HASH=\$(printf '%s' "\$RAW" | grep -o '"hash":"[A-F0-9]*"' | head -1 | cut -d'"' -f4)
if [ -z "\$RESULT" ]; then printf '%s\n' "\$RAW"; exit 1; fi
echo "trust line \${ADDR} -> genesis \${CURRENCY} limit \${LIMIT}: \${RESULT} \${HASH}"
EOF

cat > "${HARNESS_DIR}/quit" <<EOF
#!/usr/bin/env bash
tmux send-keys -t ${SESSION}:0 C-c
tmux send-keys -t ${SESSION}:1 C-c
sleep 1
tmux kill-session -t ${SESSION} 2>/dev/null || true
echo "done."
EOF

chmod +x "${HARNESS_DIR}"/*

################################################################################
# Write info file
################################################################################

cat > "${NODES_ROOT}/info.txt" <<EOF
XRP simnet harness
==================
RPC endpoint : http://127.0.0.1:${RPC_PORT}
WS endpoint  : ws://127.0.0.1:${WS_PORT}

Genesis account : rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh
  seed          : snoPBrXtMeMyMHUVTgbuqAfg1SUTb
  private key   : 1ACAAEDECE405B2A958212629E16F2EB46B153EEE94CDD350FDEFF52795525B7

Commands (run from ${HARNESS_DIR}):
  ./mine [n]          advance ledger n times (default 1)
  ./fund <addr> <xrp> fund address from genesis
  ./balance <addr>    show account balance
  ./tip               show ledger indices
  ./server            show server_state
  ./tx <hash>         look up transaction
  ./submit <blob>     submit raw tx blob
  ./issue <addr> <amount> [currency]   pay an IOU out of genesis (default USDC)
  ./newaccount [xrp]  create a funded account, printing its address and seed
  ./trustset <seed> [currency] [limit] open a trust line to genesis
  ./quit              stop everything

Issued currencies: genesis is the issuer. The wallet's simnet usdc.xrp token
names it as such, so once the wallet has established its trust line,
./issue <wallet-address> 25 delivers 25 USDC. For a counterparty:
./newaccount, then ./trustset <seed>, then ./issue <address> <amount>.
EOF

################################################################################
# Start tmux session
################################################################################

export SHELL
tmux new-session -d -s "${SESSION}"

# Window 0: rippled (local binary or Docker container)
tmux rename-window -t "${SESSION}:0" "rippled"
if [ "${USE_DOCKER}" = "0" ]; then
  tmux send-keys -t "${SESSION}:0" \
    "${RIPPLED_BIN} --standalone --start --conf ${ALPHA_DIR}/rippled.cfg" C-m
else
  tmux send-keys -t "${SESSION}:0" \
    "docker run --rm --name xrp-simnet --platform linux/amd64 -p 127.0.0.1:${RPC_PORT}:5005 -p 127.0.0.1:${WS_PORT}:6006 -v ${DOCKER_CFG_DIR}:/config/ ${DOCKER_IMAGE} --standalone --start" C-m
fi

# Window 1: tick loop (started after rippled is ready)
tmux new-window -t "${SESSION}:1" -n "tick"

# Window 2: harness-ctl shell
tmux new-window -t "${SESSION}:2" -n "harness-ctl"
tmux send-keys -t "${SESSION}:2" "cd ${HARNESS_DIR}" C-m

################################################################################
# Wait for rippled to be ready
################################################################################

echo "Waiting for rippled on port ${RPC_PORT}..."
for i in $(seq 1 30); do
  if curl -sf -X POST "http://127.0.0.1:${RPC_PORT}" \
      -H "Content-Type: application/json" \
      -d '{"method":"server_info","params":[{}]}' > /dev/null 2>&1; then
    echo "rippled is ready."
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "rippled did not start within 60 seconds." >&2
    exit 1
  fi
  sleep 2
done

################################################################################
# Start tick loop
################################################################################

tmux send-keys -t "${SESSION}:1" "${HARNESS_DIR}/tick-loop" C-m

################################################################################
# Print info and attach
################################################################################

cat "${NODES_ROOT}/info.txt"
echo ""
echo "Harness ready. Attaching to tmux session '${SESSION}'."
echo "  window 0: rippled"
echo "  window 1: tick loop"
echo "  window 2: harness-ctl"
echo ""

HARNESS_STARTED=1
tmux select-window -t "${SESSION}:2"
tmux attach-session -t "${SESSION}"
