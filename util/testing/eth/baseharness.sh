SESSION="${CHAIN}-harness"

# TESTING_ADDRESS is used by the client's internal node.
TESTING_ADDRESS="946dfaB1AD7caCFeF77dE70ea68819a30acD4577"

fileToHex () {
  if [ ! -f "$1" ]; then
    echo "fileToHex: contract bytecode file not found: $1" >&2
    exit 1
  fi
  echo $(xxd -p "$1" | tr -d '\n')
}
# fileToHex exits 1 in a subshell on a missing file; the `|| exit 1` propagates
# that to this script, which otherwise has no `set -e` to abort the assignment.
TEST_TOKEN=$(fileToHex "../../../wallet/asset/erc20/contracts/token_contract.bin") || exit 1
MULTIBALANCE_BIN=$(fileToHex "../../../wallet/asset/eth/contracts/multibalance/contract.bin") || exit 1

# Ensure we can create the session and that there's not a session already
# running before we nuke the data directory.
tmux new-session -d -s $SESSION "${SHELL}"

if [ -d "${NODES_ROOT}" ]; then
  rm -R "${NODES_ROOT}"
fi

mkdir -p "${NODES_ROOT}/alpha"
mkdir -p "${NODES_ROOT}/harness-ctl"

echo "Writing ctl scripts"
################################################################################
# Control Scripts
################################################################################

cat > "${NODES_ROOT}/harness-ctl/send.js" <<EOF
function send(to, value) {
  to = to.startsWith('0x') ? to : '0x' + to
  return eth.sendTransaction({from:eth.accounts[0], to, value})
}
EOF

cat > "${NODES_ROOT}/harness-ctl/sendtoaddress" <<EOF
#!/usr/bin/env bash
"${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/send.js --exec send(\"\$1\",\$2*1e18)"
EOF
chmod +x "${NODES_ROOT}/harness-ctl/sendtoaddress"

cat > "${NODES_ROOT}/harness-ctl/deploy.js" <<EOF
function deploy(contract) {
  tx = eth.sendTransaction({from:eth.accounts[0],data:"0x"+contract})
  return tx;
}

function deployERC20(contract, decimals) {
  const hexDecimals = decimals.toString(16);
  const data = "0x" + contract + hexDecimals.padStart(64, "0");
  tx = eth.sendTransaction({from:eth.accounts[0],data:data})
  return tx;
}
EOF

cat > "${NODES_ROOT}/harness-ctl/contractAddress.js" <<EOF
function contractAddress(tx) {
  addr = eth.getTransactionReceipt(tx).contractAddress
  return addr;
}
EOF

# Add node script.
HARNESS_DIR=$(realpath ../eth/)
cp "${HARNESS_DIR}/create-node.sh" "${NODES_ROOT}/harness-ctl/create-node"

# Shutdown script
cat > "${NODES_ROOT}/harness-ctl/quit" <<EOF
#!/usr/bin/env bash
if tmux list-windows -t $SESSION 2>/dev/null | grep -q relay; then
  tmux send-keys -t $SESSION:relay C-c
fi
tmux send-keys -t $SESSION:5 C-c
tmux send-keys -t $SESSION:1 C-c
tmux kill-session
EOF
chmod +x "${NODES_ROOT}/harness-ctl/quit"

################################################################################
# Start harness
################################################################################

tmux rename-window -t $SESSION:0 'harness-ctl'
tmux send-keys -t $SESSION:0 "set +o history" C-m
tmux send-keys -t $SESSION:0 "cd ${NODES_ROOT}/harness-ctl" C-m

################################################################################
# Eth nodes
################################################################################

"${NODES_ROOT}/harness-ctl/create-node" "$SESSION:1" "alpha" \
	"$ALPHA_AUTHRPC_PORT" "$ALPHA_HTTP_PORT" "$ALPHA_WS_PORT" \
	"eth"

sleep 20

SEND_AMT=5000000000000000000000
echo "Sending 5000 to testing"
TEST_TX_HASH=$("${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/send.js --exec send(\"${TESTING_ADDRESS}\",${SEND_AMT})" | sed 's/"//g')
echo "ETH transaction to use in tests is ${TEST_TX_HASH}. Saving to ${NODES_ROOT}/test_tx_hash.txt"
cat > "${NODES_ROOT}/test_tx_hash.txt" <<EOF
${TEST_TX_HASH}
EOF

# gethDeploy writes the deploy JS expression to a preload file to avoid
# command-line length limits with large contract bytecodes.
gethDeploy() {
  local jsExpr="$1"
  cat > "${NODES_ROOT}/harness-ctl/_deploy_cmd.js" <<DEPLOYEOF
var __result = ${jsExpr};
DEPLOYEOF
  "${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/deploy.js,${NODES_ROOT}/harness-ctl/_deploy_cmd.js --exec __result" | sed 's/"//g'
}

echo "Deploying USDC contract."
TEST_USDC_CONTRACT_HASH=$(gethDeploy "deployERC20(\"${TEST_TOKEN}\",6)")

echo "Deploying USDT contract."
TEST_USDT_CONTRACT_HASH=$(gethDeploy "deployERC20(\"${TEST_TOKEN}\",6)")

echo "Deploying LINK contract."
TEST_LINK_CONTRACT_HASH=$(gethDeploy "deployERC20(\"${TEST_TOKEN}\",18)")

echo "Deploying PAXG contract."
TEST_PAXG_CONTRACT_HASH=$(gethDeploy "deployERC20(\"${TEST_TOKEN}\",18)")

echo "Deploying MultiBalance contract."
MULTIBALANCE_CONTRACT_HASH=$(gethDeploy "deploy(\"${MULTIBALANCE_BIN}\")")

mine_pending_txs() {
  while true
  do
    TXSLEN=$("${NODES_ROOT}/harness-ctl/alpha" "attach --exec eth.pendingTransactions.length")
    if [ "$TXSLEN" -eq 0 ]; then
      break
    fi
    echo "Waiting for transactions to be mined."
    sleep 2
  done
}

mine_pending_txs

TEST_USDC_CONTRACT_ADDR=$("${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/contractAddress.js --exec contractAddress(\"${TEST_USDC_CONTRACT_HASH}\")" | sed 's/"//g')
echo "Test USDC contract address is ${TEST_USDC_CONTRACT_ADDR}. Saving to ${NODES_ROOT}/test_usdc_contract_address.txt"
cat > "${NODES_ROOT}/test_usdc_contract_address.txt" <<EOF
${TEST_USDC_CONTRACT_ADDR}
EOF

TEST_USDT_CONTRACT_ADDR=$("${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/contractAddress.js --exec contractAddress(\"${TEST_USDT_CONTRACT_HASH}\")" | sed 's/"//g')
echo "Test USDT contract address is ${TEST_USDT_CONTRACT_ADDR}. Saving to ${NODES_ROOT}/test_usdt_contract_address.txt"
cat > "${NODES_ROOT}/test_usdt_contract_address.txt" <<EOF
${TEST_USDT_CONTRACT_ADDR}
EOF

TEST_LINK_CONTRACT_ADDR=$("${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/contractAddress.js --exec contractAddress(\"${TEST_LINK_CONTRACT_HASH}\")" | sed 's/"//g')
echo "Test LINK contract address is ${TEST_LINK_CONTRACT_ADDR}. Saving to ${NODES_ROOT}/test_link_contract_address.txt"
cat > "${NODES_ROOT}/test_link_contract_address.txt" <<EOF
${TEST_LINK_CONTRACT_ADDR}
EOF

TEST_PAXG_CONTRACT_ADDR=$("${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/contractAddress.js --exec contractAddress(\"${TEST_PAXG_CONTRACT_HASH}\")" | sed 's/"//g')
echo "Test PAXG contract address is ${TEST_PAXG_CONTRACT_ADDR}. Saving to ${NODES_ROOT}/test_paxg_contract_address.txt"
cat > "${NODES_ROOT}/test_paxg_contract_address.txt" <<EOF
${TEST_PAXG_CONTRACT_ADDR}
EOF

cat > "${NODES_ROOT}/harness-ctl/loadTestToken.js" <<EOF
    // This ABI comes from running 'solc --abi TestToken.sol'
    const testTokenABI = [{"inputs":[],"stateMutability":"nonpayable","type":"constructor"},{"inputs":[{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"airdrop","outputs":[],"stateMutability":"nonpayable","type":"function"},{"inputs":[{"internalType":"address","name":"owner","type":"address"},{"internalType":"address","name":"spender","type":"address"}],"name":"allowance","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},{"inputs":[{"internalType":"address","name":"spender","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"approve","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"},{"inputs":[{"internalType":"address","name":"account","type":"address"}],"name":"balanceOf","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"name","outputs":[{"internalType":"string","name":"","type":"string"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"symbol","outputs":[{"internalType":"string","name":"","type":"string"}],"stateMutability":"view","type":"function"},{"inputs":[{"internalType":"address","name":"user","type":"address"},{"internalType":"address","name":"spender","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"testApprove","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"},{"inputs":[],"name":"totalSupply","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},{"inputs":[{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"},{"inputs":[{"internalType":"address","name":"sender","type":"address"},{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"transferFrom","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}]
    var contract = web3.eth.contract(testTokenABI)
    web3.eth.defaultAccount = web3.eth.accounts[0]

    function transfer (tokenAddr, decimals, addr, val) {
      addr = addr.startsWith('0x') ? addr : '0x'+addr
      var testToken = contract.at(tokenAddr)
      return testToken.transfer(addr, val*(10**decimals))
    }

    function airdrop (tokenAddr, amt) {
      var testToken = contract.at(tokenAddr)
      return testToken.airdrop(web3.eth.accounts[0], amt)
    }
EOF


cat > "${NODES_ROOT}/harness-ctl/alphaWithToken.sh" <<EOF
  # The testToken variable will provide access to the deployed test token contract.
  ./alpha --preload loadTestToken.js
EOF
chmod +x "${NODES_ROOT}/harness-ctl/alphaWithToken.sh"

cat > "${NODES_ROOT}/harness-ctl/sendUSDC" <<EOF
#!/usr/bin/env bash
./alpha attach --preload loadTestToken.js --exec "transfer(\"${TEST_USDC_CONTRACT_ADDR}\",6,\"\$1\",\$2)"
EOF
chmod +x "${NODES_ROOT}/harness-ctl/sendUSDC"

cat > "${NODES_ROOT}/harness-ctl/sendUSDT" <<EOF
#!/usr/bin/env bash
./alpha attach --preload loadTestToken.js --exec "transfer(\"${TEST_USDT_CONTRACT_ADDR}\",6,\"\$1\",\$2)"
EOF
chmod +x "${NODES_ROOT}/harness-ctl/sendUSDT"

cat > "${NODES_ROOT}/harness-ctl/sendLINK" <<EOF
#!/usr/bin/env bash
./alpha attach --preload loadTestToken.js --exec "transfer(\"${TEST_LINK_CONTRACT_ADDR}\",18,\"\$1\",\$2)"
EOF
chmod +x "${NODES_ROOT}/harness-ctl/sendLINK"

cat > "${NODES_ROOT}/harness-ctl/sendPAXG" <<EOF
#!/usr/bin/env bash
./alpha attach --preload loadTestToken.js --exec "transfer(\"${TEST_PAXG_CONTRACT_ADDR}\",18,\"\$1\",\$2)"
EOF
chmod +x "${NODES_ROOT}/harness-ctl/sendPAXG"

mine_pending_txs

MULTIBALANCE_CONTRACT_ADDR=$("${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/contractAddress.js --exec contractAddress(\"${MULTIBALANCE_CONTRACT_HASH}\")" | sed 's/"//g')
echo "MultiBalance contract address is ${MULTIBALANCE_CONTRACT_ADDR}. Saving to ${NODES_ROOT}/multibalance_address.txt"
cat > "${NODES_ROOT}/multibalance_address.txt" <<EOF
${MULTIBALANCE_CONTRACT_ADDR}
EOF

# Add test tokens.
"${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/loadTestToken.js --exec airdrop(\"${TEST_USDC_CONTRACT_ADDR}\",4400000000000000000)"  # 4.4 trillion USDC x 10^6 (6 decimals)
"${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/loadTestToken.js --exec airdrop(\"${TEST_USDT_CONTRACT_ADDR}\",4400000000000000000)"  # 4.4 trillion USDT x 10^6 (6 decimals)
"${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/loadTestToken.js --exec airdrop(\"${TEST_LINK_CONTRACT_ADDR}\",4400000000000000000000)"  # 4400 LINK x 10^18 (18 decimals)
"${NODES_ROOT}/harness-ctl/alpha" "attach --preload ${NODES_ROOT}/harness-ctl/loadTestToken.js --exec airdrop(\"${TEST_PAXG_CONTRACT_ADDR}\",4400000000000000000000)"  # 4400 PAXG x 10^18 (18 decimals)

cd "${NODES_ROOT}/harness-ctl"

TEST_BLOCK1_HASH=$("${NODES_ROOT}/harness-ctl/alpha" "attach --exec eth.getHeaderByNumber(1).hash" | sed 's/"//g')
echo "ETH block 1 hash to use in tests is ${TEST_BLOCK1_HASH}. Saving to ${NODES_ROOT}/test_block1_hash.txt"
cat > "${NODES_ROOT}/test_block1_hash.txt" <<EOF
${TEST_BLOCK1_HASH}
EOF

# Shared relay server for all EVM harnesses.
RELAY_PRIVKEY="4bd88e0fd6769a529930286c76ecc9a5dbf7b3b29f3c6fab1b27e5d99bcb4753"
RELAY_ETH_ADDR="0xfC09662648F682Ee158B16408dbc2Ca6567bA2Ad"
RELAY_CONFIG=~/dextest/evm-relay-config.json

# Rebuild the shared relay config from the known harness RPC ports instead of
# accumulating per-chain fragment files.
mkdir -p ~/dextest
CHAIN_ENTRIES=""
append_relay_chain_entry() {
  local chain_id="$1"
  local rpc_port="$2"
  local entry="{\"chainID\": ${chain_id}, \"rpcURL\": \"http://localhost:${rpc_port}\"}"
  if [ -n "${CHAIN_ENTRIES}" ]; then
    CHAIN_ENTRIES="${CHAIN_ENTRIES},${entry}"
  else
    CHAIN_ENTRIES="${entry}"
  fi
}

if nc -z localhost 38556 2>/dev/null; then
  append_relay_chain_entry 1 38556
fi
if nc -z localhost 39556 2>/dev/null; then
  append_relay_chain_entry 8453 39556
fi
if nc -z localhost 48296 2>/dev/null; then
  append_relay_chain_entry 137 48296
fi

cat > "${RELAY_CONFIG}" <<RELAYEOF
{
  "addr": ":21232",
  "privkey": "${RELAY_PRIVKEY}",
  "profitPerGas": 1.5,
  "chains": [${CHAIN_ENTRIES}]
}
RELAYEOF

# Build the relay binary.
REPO_ROOT=$(cd "${HARNESS_DIR}/../../.." && pwd)
go build -C "${REPO_ROOT}" -o ~/dextest/evmrelay ./util/evmrelay/cmd/evmrelay

# Stop existing relay if running, then (re)start with updated config.
if nc -z localhost 21232 2>/dev/null; then
    pkill -xf ".*dextest/evmrelay --config.*" || true
    sleep 1
fi

tmux new-window -t $SESSION -n "relay"
tmux send-keys -t $SESSION:relay "set +o history" C-m
tmux send-keys -t $SESSION:relay "~/dextest/evmrelay \
    --config ${RELAY_CONFIG} --simnet --log debug \
    2>&1 | tee ~/dextest/relay.log" C-m
# Wait for relay to come up.
for i in $(seq 1 10); do
    sleep 1
    nc -z localhost 21232 2>/dev/null && break
done

# Fund the relay address on this chain. The relay only needs gas money,
# not swap collateral, so 100 ETH is sufficient.
"${NODES_ROOT}/harness-ctl/sendtoaddress" "${RELAY_ETH_ADDR}" 100

# Reenable history and attach to the control session.
tmux select-window -t $SESSION:0
tmux send-keys -t $SESSION:0 "set -o history" C-m
tmux attach-session -t $SESSION
