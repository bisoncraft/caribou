#!/usr/bin/env bash
#
# Tmux script that sets up a simnet harness for Zcash on two Zebra (zebrad)
# regtest nodes.
#
# Zebra has no wallet and no command-line RPC client. The control scripts call
# the node JSON-RPC with curl and jq, and two funder wallets are kept with
# zcash-devtool, a light client that talks to the gRPC server built into
# zebrad. See README.md for installation and usage.

SYMBOL="zec"
# zebrad and zcash-devtool should be in PATH.
DAEMON="zebrad"
WALLET_TOOL="zcash-devtool"
ALPHA_LISTEN_PORT="33764"
BETA_LISTEN_PORT="33765"
ALPHA_RPC_PORT="33768"
BETA_RPC_PORT="33769"
# Light-client (lightwalletd protocol) gRPC ports, plaintext.
ALPHA_GRPC_PORT="33770"
BETA_GRPC_PORT="33771"

# BIP-39 mnemonics for the funder wallets. The addresses below are the default
# addresses of account 0 and can be re-derived with
# zcash-devtool wallet derive-address --mnemonic "..." -n regtest
# The unified addresses are not used by this script. They are recorded for
# tests that pay a funder.
ALPHA_MNEMONIC="involve hurdle oven property measure quick cotton wing next firm same core puzzle execute garden acquire dash boss harbor enlist rabbit small spare bike"
ALPHA_UNIFIED_ADDR="uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd"
ALPHA_TRANSPARENT_ADDR="tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm"

BETA_MNEMONIC="street thrive bounce giant crunch special renew cargo elegant curve tool scan spot extra lumber belt hello devote child name entire attack gather galaxy"
BETA_UNIFIED_ADDR="uregtest109f95z55t5l6xmdgdlshu60xggacda887hvxsv6kp8mpau9s8kq4n7tcydfecf7hmcev85jmt77p5hr0nn94l9d7neld8dyk3w8frq5gscfpkml39y3mdnl9gyp8jv3tqlaxq33t5hv6wz7wfq840tulv942npgwm5ulptssgcc239ududzrav2wv50ahkdl97njf6mauhqtjghy3tz"
BETA_TRANSPARENT_ADDR="tmAsfbvNQVxLcp88CBhp7AW74XdapKbpo5n"

# mine-alpha, mine-beta and the background miner pay this transparent address,
# which belongs to neither funder. The funder shield command spends every
# transparent output it holds, immature coinbase included, so paying a funder
# on every block would make its shield transactions invalid. The address is
# the default transparent address of this mnemonic:
# drop pretty present believe clump level fence loop render twenty essay visual
# pigeon salute capable sport typical betray bench supply divorce bench slot
# turtle
MINER_ADDR="tmUV6poBiYV5thY9QeovauFFYUMAejmfKNh"

# Transparent coinbase blocks mined to each funder. The subsidy is
# 2.08333333 ZEC per block, so 150 blocks is about 312.5 ZEC.
FUNDER_TRANSPARENT_BLOCKS="150"
# The shielding transaction splits its output into this many notes. Change is
# not spendable until it is mined, so the number of notes bounds the number of
# sends a funder can make in one block.
FUNDER_NOTE_COUNT="16"
# Blocks mined on top of the funding transactions. A reorg replaces the
# transactions in the blocks it removes, so a reorg deeper than this right
# after setup would leave a funder without funds.
FUNDING_BURIAL_BLOCKS="20"

# Background watch mining in window 3 by default:
# 'export NOMINER="1"' or uncomment this line to disable
#NOMINER="1"

set -e

SHELL=$(which bash)

for DEP in "${DAEMON}" "${WALLET_TOOL}" tmux curl jq awk watch; do
  if ! command -v "${DEP}" > /dev/null; then
    echo "${DEP} not found in PATH. See util/testing/zec/README.md." >&2
    exit 1
  fi
done

SESSION="${SYMBOL}-harness"
NODES_ROOT=~/dextest/${SYMBOL}

if tmux has-session -t "${SESSION}" 2> /dev/null; then
  echo "tmux session ${SESSION} is already running. Shut it down with ${NODES_ROOT}/harness-ctl/quit," >&2
  echo "or with 'tmux kill-session -t ${SESSION}' if that script is missing." >&2
  exit 1
fi

rm -rf "${NODES_ROOT}"
## Tree clean ##

################################################################################
# Make directories and start tmux
################################################################################
ALPHA_DIR="${NODES_ROOT}/alpha"
BETA_DIR="${NODES_ROOT}/beta"
WALLETS_DIR="${NODES_ROOT}/wallets"
HARNESS_DIR="${NODES_ROOT}/harness-ctl"
ACTIVATION_HEIGHTS="${NODES_ROOT}/activation-heights.toml"

mkdir -p "${ALPHA_DIR}"
mkdir -p "${BETA_DIR}"
mkdir -p "${WALLETS_DIR}/alpha"
mkdir -p "${WALLETS_DIR}/beta"
mkdir -p "${HARNESS_DIR}"

# Windows are addressed by name, so a tmux base-index setting does not matter.
cd "${NODES_ROOT}" && tmux new-session -d -s "${SESSION}" -n 'alpha' "${SHELL}"

################################################################################
# Write config files.
################################################################################

echo "Writing node config files"

# write_node_config NAME LISTEN_PORT RPC_PORT GRPC_PORT PEERS
#
# Every network upgrade through NU7 activates at height 1. Cookie
# authentication is off so the control scripts need no credentials.
# debug_enable_at_height starts the mempool without waiting for the node to
# consider itself synced.
write_node_config() {
  cat > "${NODES_ROOT}/$1/$1.toml" <<EOF
[network]
network = "Regtest"
listen_addr = "127.0.0.1:$2"
initial_testnet_peers = [$5]
peerset_initial_target_size = 1
cache_dir = false

[network.testnet_parameters.activation_heights]
NU5 = 1
NU6 = 1
"NU6.1" = 1
"NU6.2" = 1
"NU6.3" = 1
NU7 = 1

[state]
cache_dir = "${NODES_ROOT}/$1"

[rpc]
listen_addr = "127.0.0.1:$3"
lightwalletd_listen_addr = "127.0.0.1:$4"
enable_cookie_auth = false

[mining]
miner_address = "${MINER_ADDR}"

[mempool]
debug_enable_at_height = 0
EOF
}

write_node_config alpha "${ALPHA_LISTEN_PORT}" "${ALPHA_RPC_PORT}" "${ALPHA_GRPC_PORT}" ""
write_node_config beta "${BETA_LISTEN_PORT}" "${BETA_RPC_PORT}" "${BETA_GRPC_PORT}" "\"127.0.0.1:${ALPHA_LISTEN_PORT}\""

# The same schedule in the form zcash-devtool takes at wallet creation. A
# wallet whose schedule differs from the nodes' builds transactions the nodes
# reject.
cat > "${ACTIVATION_HEIGHTS}" <<EOF
overwinter = 1
sapling = 1
blossom = 1
heartwood = 1
canopy = 1
nu5 = 1
nu6 = 1
nu6_1 = 1
nu6_2 = 1
nu6_3 = 1
nu7 = 1
EOF

################################################################################
# Setup the harness-ctl directory with scripts
################################################################################

cd "${HARNESS_DIR}"

# Arguments that parse as JSON are sent typed (numbers, booleans, objects) and
# anything else is sent as a string, so both
#   ./alpha getblockhash 5
#   ./alpha getaddressbalance '{"addresses":["tm..."]}'
# work as written. getblock, getblockheader and z_gettreestate take a height
# or hash that the node requires as a string, so their first argument is never
# converted.
cat > "./rpc" <<'EOF'
#!/usr/bin/env bash
# JSON-RPC call to a node: ./rpc PORT METHOD [PARAMS...]
set -o pipefail
if [ $# -lt 2 ]; then
  echo "usage: rpc PORT METHOD [PARAMS...]" >&2
  exit 1
fi
port=$1
method=$2
shift 2
params=$(jq -cn --arg m "${method}" '
  ($m | IN("getblock", "getblockheader", "z_gettreestate")) as $string_first
  | $ARGS.positional | to_entries
  | map(.value as $a | if .key == 0 and $string_first then $a else ($a | try fromjson catch $a) end)
' --args -- "$@") || exit 1
curl -sS --data-binary "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}" \
  -H 'content-type: application/json' "http://127.0.0.1:${port}/" |
  jq -r 'if .error then ("error \(.error.code): \(.error.message)\n" | halt_error(1)) else .result end'
EOF
chmod +x "./rpc"

cat > "./alpha" <<EOF
#!/usr/bin/env bash
exec ${HARNESS_DIR}/rpc ${ALPHA_RPC_PORT} "\$@"
EOF
chmod +x "./alpha"

cat > "./beta" <<EOF
#!/usr/bin/env bash
exec ${HARNESS_DIR}/rpc ${BETA_RPC_PORT} "\$@"
EOF
chmod +x "./beta"

cat > "./mine-alpha" <<EOF
#!/usr/bin/env bash
# Mine blocks on alpha, paying the miner address: ./mine-alpha [N]
exec ${HARNESS_DIR}/alpha generate "\${1:-1}"
EOF
chmod +x "./mine-alpha"

cat > "./mine-beta" <<EOF
#!/usr/bin/env bash
# Mine blocks on beta, paying the miner address: ./mine-beta [N]
exec ${HARNESS_DIR}/beta generate "\${1:-1}"
EOF
chmod +x "./mine-beta"

cat > "./mine-to" <<EOF
#!/usr/bin/env bash
# Mine blocks on alpha with the coinbase paid to a transparent, Sapling or
# unified address: ./mine-to ADDRESS [N]
if [ \$# -lt 1 ]; then
  echo "usage: mine-to ADDRESS [N]" >&2
  exit 1
fi
exec ${HARNESS_DIR}/alpha generatetoaddress "\${2:-1}" "\$1"
EOF
chmod +x "./mine-to"

# Zebra has no disconnectnode, so the reorg is made by invalidating blocks on
# alpha and mining a longer replacement chain, which beta then follows.
# Transactions in the replaced blocks are not returned to the mempool.
cat > "./reorg" <<EOF
#!/usr/bin/env bash
# Replace the last DEPTH blocks with DEPTH+1 new ones: ./reorg [DEPTH]
set -e
depth=\${1:-1}
tip=\$(${HARNESS_DIR}/alpha getblockcount)
if ! [[ "\${depth}" =~ ^[1-9][0-9]*\$ ]] || [ "\${depth}" -ge "\${tip}" ]; then
  echo "usage: reorg [DEPTH], with DEPTH from 1 to \$((tip - 1))" >&2
  exit 1
fi
first=\$((tip - depth + 1))
hash=\$(${HARNESS_DIR}/alpha getblockhash "\${first}")
echo "Invalidating \${depth} block(s) on alpha from height \${first} (\${hash})"
${HARNESS_DIR}/alpha invalidateblock "\${hash}" > /dev/null
echo "Mining \$((depth + 1)) replacement blocks on alpha"
${HARNESS_DIR}/alpha generate "\$((depth + 1))" > /dev/null
followed=""
for _ in \$(seq 1 60); do
  if [ "\$(${HARNESS_DIR}/alpha getbestblockhash)" = "\$(${HARNESS_DIR}/beta getbestblockhash)" ]; then
    followed="1"
    break
  fi
  sleep 0.5
done
echo "alpha: \$(${HARNESS_DIR}/alpha getblockcount) \$(${HARNESS_DIR}/alpha getbestblockhash)"
echo "beta:  \$(${HARNESS_DIR}/beta getblockcount) \$(${HARNESS_DIR}/beta getbestblockhash)"
if [ -z "\${followed}" ]; then
  echo "beta has not followed alpha's new chain" >&2
  exit 1
fi
EOF
chmod +x "./reorg"

# write_wallet_scripts NAME GRPC_PORT
write_wallet_scripts() {
  cat > "./wallet-$1" <<EOF
#!/usr/bin/env bash
# ${WALLET_TOOL} wallet command for the $1 funder:
#   ./wallet-$1 SUBCOMMAND [ARGS...]
# The server and identity flags are added for sync, enhance, get-info, send
# and shield. Other subcommands that need them take them on the command line.
# Logging is at warn level unless RUST_LOG is set.
export RUST_LOG=\${RUST_LOG:-warn}
if [ \$# -lt 1 ]; then
  exec ${WALLET_TOOL} wallet --help
fi
cmd=\$1
shift
case "\${cmd}" in
  sync|enhance|get-info)
    extra=(-s 127.0.0.1:$2 --connection direct) ;;
  send|shield)
    extra=(-i ${WALLETS_DIR}/$1/age-identity.txt -s 127.0.0.1:$2 --connection direct) ;;
  *)
    extra=() ;;
esac
exec ${WALLET_TOOL} wallet -w ${WALLETS_DIR}/$1 "\${cmd}" "\${extra[@]}" "\$@"
EOF
  chmod +x "./wallet-$1"

  cat > "./send-$1" <<EOF
#!/usr/bin/env bash
# Send from the $1 funder to a transparent, Sapling or unified address:
#   ./send-$1 ADDRESS AMOUNT_ZEC [MEMO]
# Prints the transaction id on the last line. Notes with one confirmation are
# spendable.
set -e
if [ \$# -lt 2 ]; then
  echo "usage: send-$1 ADDRESS AMOUNT_ZEC [MEMO]" >&2
  exit 1
fi
if ! [[ "\$2" =~ ^[0-9]+(\\.[0-9]{1,8})?\$ ]]; then
  echo "AMOUNT_ZEC must be a decimal number with at most 8 decimal places, such as 1.5" >&2
  exit 1
fi
zats=\$(awk -v v="\$2" 'BEGIN { printf "%.0f", v * 100000000 }')
${HARNESS_DIR}/wallet-$1 sync
${HARNESS_DIR}/wallet-$1 send --address "\$1" --value "\${zats}" --min-confirmations 1 \${3:+--memo "\$3"}
EOF
  chmod +x "./send-$1"

  cat > "./balance-$1" <<EOF
#!/usr/bin/env bash
# Sync the $1 funder and print its balances in ZEC.
set -eo pipefail
${HARNESS_DIR}/wallet-$1 sync
${HARNESS_DIR}/wallet-$1 balance --json --min-confirmations 1 |
  jq 'with_entries(if .key == "chain_tip_height" then . else .value /= 100000000 end)'
EOF
  chmod +x "./balance-$1"
}

write_wallet_scripts alpha "${ALPHA_GRPC_PORT}"
write_wallet_scripts beta "${BETA_GRPC_PORT}"

# The nodes are stopped with the stop RPC. A Ctrl-C sent with tmux send-keys
# only leaves copy mode when a node window has been scrolled back, and the
# wait-for below would then never return.
cat > "./quit" <<EOF
#!/usr/bin/env bash
# Stop the miner and both nodes, then close the tmux session.
if ! tmux has-session -t ${SESSION} 2> /dev/null; then
  echo "tmux session ${SESSION} is not running" >&2
  exit 1
fi
EOF
if [ -z "$NOMINER" ] ; then
  echo "tmux send-keys -t ${SESSION}:miner C-c" >> "./quit"
fi
cat >> "./quit" <<EOF
${HARNESS_DIR}/alpha stop > /dev/null
${HARNESS_DIR}/beta stop > /dev/null
tmux wait-for alpha${SYMBOL}
tmux wait-for beta${SYMBOL}
# seppuku
tmux kill-session -t ${SESSION}
EOF
chmod +x "./quit"

################################################################################
# Start the nodes
################################################################################

# A failed setup step leaves the session and the nodes running so that the
# node windows can be read.
setup_failed() {
  echo "Setup failed. Node output is in tmux session ${SESSION}: tmux attach -t ${SESSION}" >&2
  echo "Shut it down with ${HARNESS_DIR}/quit" >&2
}
trap setup_failed EXIT

# wait_for SECONDS DESCRIPTION COMMAND [ARGS...] polls until the command
# succeeds or the time is up.
wait_for() {
  local limit=$1 desc=$2
  shift 2
  for _ in $(seq 1 $((limit * 2))); do
    if "$@" > /dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "Timed out waiting for ${desc}" >&2
  exit 1
}

beta_has_peer() {
  [ "$(./beta getpeerinfo | jq 'length')" -ge 1 ]
}

tips_match() {
  [ "$(./alpha getbestblockhash)" = "$(./beta getbestblockhash)" ]
}

# Blocks normally reach the other node within a second or two. A node that
# falls back to its sync loop, or that has to reconnect, can take about 90
# seconds.
SYNC_TIMEOUT="120"

tmux send-keys -t "${SESSION}:alpha" "set +o history" C-m
tmux send-keys -t "${SESSION}:alpha" "cd ${ALPHA_DIR}" C-m

echo "Starting simnet alpha node"
tmux send-keys -t "${SESSION}:alpha" "${DAEMON} -c ${ALPHA_DIR}/alpha.toml start; tmux wait-for -S alpha${SYMBOL}" C-m
# zebrad opens its peer listener before its RPC server, so alpha is accepting
# connections by the time beta starts.
wait_for 30 "the alpha RPC server" ./alpha getblockcount

tmux new-window -t "${SESSION}:" -n 'beta' "${SHELL}"
tmux send-keys -t "${SESSION}:beta" "set +o history" C-m
tmux send-keys -t "${SESSION}:beta" "cd ${BETA_DIR}" C-m

echo "Starting simnet beta node"
tmux send-keys -t "${SESSION}:beta" "${DAEMON} -c ${BETA_DIR}/beta.toml start; tmux wait-for -S beta${SYMBOL}" C-m
wait_for 30 "the beta RPC server" ./beta getblockcount
# A node that starts behind is slow to catch up, so beta must be connected
# before any blocks are mined.
wait_for "${SYNC_TIMEOUT}" "beta to connect to alpha" beta_has_peer

tmux new-window -t "${SESSION}:" -n 'harness-ctl' "${SHELL}"
tmux send-keys -t "${SESSION}:harness-ctl" "cd ${HARNESS_DIR}" C-m

################################################################################
# Mine
################################################################################

echo "Mining ${FUNDER_TRANSPARENT_BLOCKS} transparent coinbase blocks to each funder"
./mine-to "${ALPHA_TRANSPARENT_ADDR}" "${FUNDER_TRANSPARENT_BLOCKS}" > /dev/null
./mine-to "${BETA_TRANSPARENT_ADDR}" "${FUNDER_TRANSPARENT_BLOCKS}" > /dev/null

# Transparent coinbase matures after 100 blocks.
echo "Mining 100 blocks to mature the funders' transparent coinbase"
./mine-alpha 100 > /dev/null

wait_for "${SYNC_TIMEOUT}" "beta to sync with alpha" tips_match

################################################################################
# Create and fund the funder wallets
################################################################################

# init_wallet NAME MNEMONIC GRPC_PORT
#
# The birthday is 2 because a scan starting at height 1 asks the server for
# the tree state at height 0, which Zebra rejects.
init_wallet() {
  echo "$2" | RUST_LOG=warn "${WALLET_TOOL}" wallet -w "${WALLETS_DIR}/$1" init --name "$1" \
    -i "${WALLETS_DIR}/$1/age-identity.txt" --birthday 2 -n regtest \
    -s "127.0.0.1:$3" --connection direct --activation-heights "${ACTIVATION_HEIGHTS}"
}

echo "Creating the alpha funder wallet"
init_wallet alpha "${ALPHA_MNEMONIC}" "${ALPHA_GRPC_PORT}"
./wallet-alpha sync

echo "Creating the beta funder wallet"
init_wallet beta "${BETA_MNEMONIC}" "${BETA_GRPC_PORT}"
./wallet-beta sync

# Each shielding transaction is mined on the node it was submitted to, because
# a transaction takes several seconds to reach the other node's mempool.
echo "Shielding the alpha funder's transparent coinbase"
./wallet-alpha shield --target-note-count "${FUNDER_NOTE_COUNT}"
./mine-alpha 1 > /dev/null
wait_for "${SYNC_TIMEOUT}" "beta to sync with alpha" tips_match

echo "Shielding the beta funder's transparent coinbase"
./wallet-beta shield --target-note-count "${FUNDER_NOTE_COUNT}"
./mine-beta 1 > /dev/null
wait_for "${SYNC_TIMEOUT}" "alpha to sync with beta" tips_match

echo "Mining ${FUNDING_BURIAL_BLOCKS} blocks on top of the funding transactions"
./mine-alpha "${FUNDING_BURIAL_BLOCKS}" > /dev/null
wait_for "${SYNC_TIMEOUT}" "beta to sync with alpha" tips_match

echo "alpha funder balance:"
./balance-alpha
echo "beta funder balance:"
./balance-beta

################################################################################
# Setup the watch background miner, if required
################################################################################
if [ -z "$NOMINER" ] ; then
  tmux new-window -t "${SESSION}:" -n 'miner' "${SHELL}"
  tmux send-keys -t "${SESSION}:miner" "cd ${HARNESS_DIR}" C-m
  tmux send-keys -t "${SESSION}:miner" "watch -n 15 ./mine-alpha 1" C-m
fi

trap - EXIT

echo "----------"
echo "SETUP DONE"
echo "----------"

tmux select-window -t "${SESSION}:harness-ctl"
tmux attach-session -t "${SESSION}"
