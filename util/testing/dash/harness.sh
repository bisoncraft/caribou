#!/usr/bin/env bash
#
# Tmux script that sets up a simnet harness for Dash. 
#
# Wallets can be re-created using seed phrase. 'sethdseed' does not exist
# Rather you can make a Blank non HD wallet and use 'upgradetohd' using
# a "mnemonic" parameter that is a set of seed words from the hdseed of
# a previous wallet. This is really for restoring wallets but we can use
# it to make a repeatable wallet.

SYMBOL="dash"
# Dash binaries should be in PATH
DAEMON="dashd"
CLI="dash-cli"
RPC_USER="user"
RPC_PASS="pass"
ALPHA_LISTEN_PORT="20775"
BETA_LISTEN_PORT="20776"
ALPHA_RPC_PORT="20777"
BETA_RPC_PORT="20778"
WALLET_PASSWORD="abc"

# These should be BIP39 mnemomic seed words representing the wallet's hdseed.
# Using the hex hdseed directly does not seem to be possible.
#
# With no mnemonic supplied dash core will generate a random hdseed when the
# `upgradetohd' RPC is called.`
ALPHA_SEED_MNEMONIC="lecture crew glove notable rail wink artefact canvas wreck achieve render sail select rigid guide celery pluck nature brass galaxy faith hire confirm bullet"
BETA_SEED_MNEMONIC="spider truth grid loan blame wall proof again link book teach quiz sniff parrot spare reward manual flower smoke fury nest kingdom possible roof"
GAMMA_SEED_MNEMONIC="tent spend begin analyst salute flavor mind gift speak oval lunar wife during issue pull muscle hamster mosquito note benefit radar observe reveal tiny"
DELTA_SEED_MNEMONIC="great harsh elite anger thunder mandate ladder rough butter image defense elevator boss border submit matter tobacco banner rather talent prevent mother defense jump"

# ""
# ├── gamma
# │   └── wallet.dat
# └── wallet.dat
# ""
# ├── delta
# │   └── wallet.dat
# └── wallet.dat

# alpha node has an unnamed HD wallet ""  
ALPHA_MINING_ADDR="yNuBWDrd4z5X98jUbFv9QGA5kEovzr8tbF"

# beta node has an unnamed HD wallet ""  
BETA_MINING_ADDR="yXsphR4cWKFy2wyApUC5fbWPX3u8Pg6wqi"

# gamma is a named HD wallet in the alpha node's wallet directory.
GAMMA_ADDRESS="yQmfjX89287y3cYFXdDAo882VCG7cSspRm"

# delta is a named HD wallet in the beta node's wallet directory.
DELTA_ADDRESS="ySM59Zz7BsVsd8TJnNbLTXoZmkqN4Ph2Yw"

# Background watch mining in window 3 by default:  
# 'export NOMINER="1"' or uncomment this line to disable
#NOMINER="1"

set -ex

SHELL=$(which bash)

NODES_ROOT=~/dextest/${SYMBOL}
rm -rf "${NODES_ROOT}"
## Tree clean ##

# -datadir is required: Dash Core v23+ dash-cli treats a missing default config
# file (~/.dashcore/dash.conf) as a fatal "Error reading configuration file:"
# rather than a warning, so without it every harness-ctl command fails. Pointing
# at the node's (existing) datadir satisfies the config read; the RPC connection
# itself uses -rpcport/-rpcuser/-rpcpassword. gamma lives in the alpha node and
# delta in the beta node, so they use the corresponding datadir.
ALPHA_CLI_CFG="-datadir=${NODES_ROOT}/alpha -rpcwallet= -rpcport=${ALPHA_RPC_PORT} -regtest=1 -rpcuser=user -rpcpassword=pass"
BETA_CLI_CFG="-datadir=${NODES_ROOT}/beta -rpcwallet= -rpcport=${BETA_RPC_PORT} -regtest=1 -rpcuser=user -rpcpassword=pass"

GAMMA_CLI_CFG="-datadir=${NODES_ROOT}/alpha -rpcwallet=gamma -rpcport=${ALPHA_RPC_PORT} -regtest=1 -rpcuser=user -rpcpassword=pass"
DELTA_CLI_CFG="-datadir=${NODES_ROOT}/beta -rpcwallet=delta -rpcport=${BETA_RPC_PORT} -regtest=1 -rpcuser=user -rpcpassword=pass"

# DONE can be used in a send-keys call along with a `wait-for dash` command to
# wait for process termination.
DONE="; tmux wait-for -S ${SYMBOL}"
WAIT="wait-for ${SYMBOL}"

################################################################################
# Make directories and start tmux
################################################################################
ALPHA_DIR="${NODES_ROOT}/alpha"
BETA_DIR="${NODES_ROOT}/beta"
HARNESS_DIR="${NODES_ROOT}/harness-ctl"

mkdir -p "${ALPHA_DIR}"
mkdir -p "${BETA_DIR}"
mkdir -p "${HARNESS_DIR}"

SESSION="${SYMBOL}-harness"

cd ${NODES_ROOT} && tmux new-session -d -s $SESSION $SHELL

################################################################################
# Write config files.
################################################################################

echo "Writing node config files"

# These config files aren't actually used here, but can be used by other
# programs.

cat > "${ALPHA_DIR}/alpha.conf" <<EOF
rpcwallet=
rpcuser=user
rpcpassword=pass
datadir=${ALPHA_DIR}
txindex=1
port=${ALPHA_LISTEN_PORT}
regtest=1
rpcport=${ALPHA_RPC_PORT}
EOF

cat > "${BETA_DIR}/beta.conf" <<EOF
rpcwallet=
rpcuser=user
rpcpassword=pass
datadir=${BETA_DIR}
txindex=1
regtest=1
rpcport=${BETA_RPC_PORT}
EOF

################################################################################
# Start the alpha node.
################################################################################

tmux rename-window -t $SESSION:0 'alpha'
tmux send-keys -t $SESSION:0 "set +o history" C-m
tmux send-keys -t $SESSION:0 "cd ${ALPHA_DIR}" C-m

echo "Starting simnet alpha node"
tmux send-keys -t $SESSION:0 "${DAEMON} -rpcuser=user -rpcpassword=pass \
  -rpcport=${ALPHA_RPC_PORT} -datadir=${ALPHA_DIR} \
  -debug=rpc -debug=net -debug=mempool -debug=walletdb -debug=addrman -debug=mempoolrej \
  -whitelist=127.0.0.0/8 -whitelist=::1 \
  -peerblockfilters=1 -blockfilterindex=basic \
  -txindex=1 -regtest=1 -port=${ALPHA_LISTEN_PORT} -fallbackfee=0.00001 \
  ${EXTRA_ARGS}; tmux wait-for -S alpha${SYMBOL}" C-m
sleep 3

################################################################################
# Start the beta node.
################################################################################

tmux new-window -t $SESSION:1 -n 'beta' $SHELL
tmux send-keys -t $SESSION:1 "set +o history" C-m
tmux send-keys -t $SESSION:1 "cd ${BETA_DIR}" C-m

echo "Starting simnet beta node"
tmux send-keys -t $SESSION:1 "${DAEMON} -rpcuser=user -rpcpassword=pass \
  -rpcport=${BETA_RPC_PORT} -datadir=${BETA_DIR} \
  -debug=rpc -debug=net -debug=mempool -debug=walletdb -debug=addrman -debug=mempoolrej \
  -whitelist=127.0.0.0/8 -whitelist=::1 \
  -peerblockfilters=1 -blockfilterindex=basic \
  -txindex=1 -regtest=1 -port=${BETA_LISTEN_PORT} -fallbackfee=0.00001 \
  ${EXTRA_ARGS}; tmux wait-for -S beta${SYMBOL}" C-m
sleep 3

################################################################################
# Setup the harness-ctl directory with scripts
################################################################################

tmux new-window -t $SESSION:2 -n 'harness-ctl' $SHELL
tmux send-keys -t $SESSION:2 "set +o history" C-m
tmux send-keys -t $SESSION:2 "cd ${HARNESS_DIR}" C-m
sleep 1

cd ${HARNESS_DIR}

cat > "./alpha" <<EOF
#!/usr/bin/env bash
${CLI} ${ALPHA_CLI_CFG} "\$@"
EOF
chmod +x "./alpha"

cat > "./mine-alpha" <<EOF
#!/usr/bin/env bash
${CLI} ${ALPHA_CLI_CFG} generatetoaddress \$1 ${ALPHA_MINING_ADDR}
EOF
chmod +x "./mine-alpha"

cat > "./beta" <<EOF
#!/usr/bin/env bash
${CLI} ${BETA_CLI_CFG} "\$@"
EOF
chmod +x "./beta"

cat > "./mine-beta" <<EOF
#!/usr/bin/env bash
${CLI} ${BETA_CLI_CFG} generatetoaddress \$1 ${BETA_MINING_ADDR}
EOF
chmod +x "./mine-beta"

cat > "./gamma" <<EOF
#!/usr/bin/env bash
${CLI} ${GAMMA_CLI_CFG} "\$@"
EOF
chmod +x "./gamma"

cat > "./delta" <<EOF
#!/usr/bin/env bash
${CLI} ${DELTA_CLI_CFG} "\$@"
EOF
chmod +x "./delta"

cat > "./reorg" <<EOF
#!/usr/bin/env bash
set -x
echo "Disconnecting beta from alpha"
sleep 1
./beta disconnectnode 127.0.0.1:${ALPHA_LISTEN_PORT}
echo "Mining a block on alpha"
sleep 1
./mine-alpha 1
echo "Mining 3 blocks on beta"
./mine-beta 3
sleep 2
echo "Reconnecting beta to alpha"
./beta addnode 127.0.0.1:${ALPHA_LISTEN_PORT} onetry
sleep 2
EOF
chmod +x "./reorg"

cat > "./new-wallet" <<EOF
#!/usr/bin/env bash
echo "making a new named non-hd wallet on "\$1" node: name="\$2" password=none"
"./\$1" createwallet "\$2" false false
EOF
chmod +x "./new-wallet"

cat > "${HARNESS_DIR}/quit" <<EOF
#!/usr/bin/env bash
tmux send-keys -t $SESSION:0 C-c
tmux send-keys -t $SESSION:1 C-c
if [ -z "$NOMINER" ] ; then
  tmux send-keys -t $SESSION:3 C-c
fi
tmux wait-for alpha${SYMBOL}
tmux wait-for beta${SYMBOL}
# seppuku
tmux kill-session
EOF
chmod +x "${HARNESS_DIR}/quit"

# Registers a regular masternode funded from gamma. The ProRegTx creates the
# 1000 DASH collateral output (internal collateral). The operator BLS key is
# generated here and no masternode process is run, so the node is only
# registered on chain. Usage: ./mn-register [core-p2p-port]
cat > "${HARNESS_DIR}/mn-register" <<EOF
#!/usr/bin/env bash
set -e
PORT=\${1:-19999}
./gamma walletpassphrase "${WALLET_PASSWORD}" 600
BLS=\$(./gamma bls generate)
OPERATOR_SECRET=\$(echo "\$BLS" | sed -n 's/.*"secret": "\(.*\)".*/\1/p')
OPERATOR_PUBLIC=\$(echo "\$BLS" | sed -n 's/.*"public": "\(.*\)".*/\1/p')
COLLATERAL=\$(./gamma getnewaddress)
OWNER=\$(./gamma getnewaddress)
VOTING=\$(./gamma getnewaddress)
PAYOUT=\$(./gamma getnewaddress)
FUND=\$(./gamma getnewaddress)
./gamma sendtoaddress "\$FUND" 1010 > /dev/null
./mine-alpha 1 > /dev/null
PROTX=\$(./gamma protx register_fund "\$COLLATERAL" "[\\"127.0.0.1:\$PORT\\"]" "\$OWNER" "\$OPERATOR_PUBLIC" "\$VOTING" 0 "\$PAYOUT" "\$FUND")
./mine-alpha 1 > /dev/null
echo "proTxHash=\$PROTX"
echo "operatorSecret=\$OPERATOR_SECRET"
echo "operatorPublic=\$OPERATOR_PUBLIC"
echo "collateral=\$COLLATERAL owner=\$OWNER voting=\$VOTING payout=\$PAYOUT"
EOF
chmod +x "${HARNESS_DIR}/mn-register"

################################################################################
# Connect alpha node to beta
################################################################################
echo "connect alpha node to beta"

tmux send-keys -t $SESSION:2 "./beta addnode 127.0.0.1:${ALPHA_LISTEN_PORT} add${DONE}" C-m\; ${WAIT}
# Give the nodes time to sync.
sleep 2

tmux send-keys -t $SESSION:2 "./beta getnetworkinfo${DONE}" C-m\; ${WAIT}

################################################################################
# Have to generate a block before calling upgradetohd
################################################################################
echo "Generating the genesis block"

tmux send-keys -t $SESSION:2 "./alpha generatetoaddress 1 ${ALPHA_MINING_ADDR}${DONE}" C-m\; ${WAIT}
sleep 2

################################
# Create HD wallets from seeds #
################################

# https://docs.dash.org/projects/core/en/stable/docs/api/remote-procedure-calls-wallet.html#createwallet
# https://docs.dash.org/projects/core/en/stable/docs/api/remote-procedure-calls-wallet.html#upgradetohd

################################################################################
# Create unnamed wallet on node alpha
################################################################################
echo "Creating alpha wallet - encrypted HD"

tmux send-keys -t $SESSION:2 "./alpha createwallet \"\" false true \"${WALLET_PASSWORD}\" ${DONE}" C-m\; ${WAIT}
sleep 2

tmux send-keys -t $SESSION:2 "./alpha  upgradetohd \"${ALPHA_SEED_MNEMONIC}\" \"\" \"${WALLET_PASSWORD}\"${DONE}" C-m\; ${WAIT}
sleep 3

################################################################################
# Create unnamed wallet on node beta
################################################################################
echo "Creating beta wallet - encrypted HD"

tmux send-keys -t $SESSION:2 "./beta  createwallet \"\" false true \"${WALLET_PASSWORD}\" ${DONE}" C-m\; ${WAIT}
sleep 2

tmux send-keys -t $SESSION:2 "./beta  upgradetohd \"${BETA_SEED_MNEMONIC}\" \"\" ${WALLET_PASSWORD}${DONE}" C-m\; ${WAIT}
sleep 3

################################################################################
# Create the gamma wallet
################################################################################
echo "Creating gamma wallet - encrypted HD"

tmux send-keys -t $SESSION:2 "./alpha createwallet \"gamma\" false true \"${WALLET_PASSWORD}\" ${DONE}" C-m\; ${WAIT}
sleep 2

tmux send-keys -t $SESSION:2 "./alpha -rpcwallet=\"gamma\" upgradetohd \"${GAMMA_SEED_MNEMONIC}\" \"\" \"${WALLET_PASSWORD}\"${DONE}" C-m\; ${WAIT}
sleep 3

################################################################################
# Create the delta wallet
################################################################################
echo "Creating delta wallet - encrypted HD"

tmux send-keys -t $SESSION:2 "./beta createwallet \"delta\" false true \"${WALLET_PASSWORD}\" ${DONE}" C-m\; ${WAIT}
sleep 3

tmux send-keys -t $SESSION:2 "./beta -rpcwallet=\"delta\" upgradetohd \"${DELTA_SEED_MNEMONIC}\" \"\" \"${WALLET_PASSWORD}\"${DONE}" C-m\; ${WAIT}
sleep 3

################################################################################
# Mine alpha
################################################################################
echo "Generating 200 blocks for alpha"

tmux send-keys -t $SESSION:2 "./alpha generatetoaddress 100 ${ALPHA_MINING_ADDR}${DONE}" C-m\; ${WAIT}
sleep 1
tmux send-keys -t $SESSION:2 "./alpha generatetoaddress 100 ${ALPHA_MINING_ADDR}${DONE}" C-m\; ${WAIT}
sleep 1

################################################################################
# Send beta, gamma and delta some coin
################################################################################
echo "Funding beta, gamma & delta wallets from alpha's block subsidy. 500 tDASH/block."

tmux send-keys -t $SESSION:2 "./alpha walletpassphrase \"${WALLET_PASSWORD}\" 100000000${i}${DONE}" C-m\; ${WAIT}

#  Send some dash to beta, gamma and delta wallets, mining a few more blocks.
echo "Sending 8777 DASH each to beta, gamma and delta in 8 blocks"
for i in 1000 1800 500 700 100 1500 300 2500 200 100 7 7 7 7 7 7 7 3 3 3 3 3 3 2 2 2 2 2 1
do
  tmux send-keys -t $SESSION:2 "./alpha sendtoaddress ${BETA_MINING_ADDR} ${i}${DONE}" C-m\; ${WAIT}
  tmux send-keys -t $SESSION:2 "./alpha sendtoaddress ${GAMMA_ADDRESS} ${i}${DONE}" C-m\; ${WAIT}
  tmux send-keys -t $SESSION:2 "./alpha sendtoaddress ${DELTA_ADDRESS} ${i}${DONE}" C-m\; ${WAIT}
  #
  tmux send-keys -t $SESSION:2 "./mine-beta 1${DONE}" C-m\; ${WAIT}
done

################################################################################
# Mine past the regtest DIP-3 enforcement height (500) so masternodes can be
# registered. DIP-3 activates at 432, but until it is enforced a ProRegTx must
# use the same key for owner and voting ("bad-protx-key-not-same").
################################################################################
echo "Generating 290 blocks for alpha to pass DIP-3 enforcement"

tmux send-keys -t $SESSION:2 "./alpha generatetoaddress 290 ${ALPHA_MINING_ADDR}${DONE}" C-m\; ${WAIT}
sleep 1

################################################################################
# Setup watch background miner -- if required
################################################################################
if [ -z "$NOMINER" ] ; then
  tmux new-window -t $SESSION:3 -n "miner" $SHELL
  tmux send-keys -t $SESSION:3 "cd ${HARNESS_DIR}" C-m
  tmux send-keys -t $SESSION:3 "watch -n 15 ./mine-alpha 1" C-m
fi

set +x

echo "----------"
echo "SETUP DONE"
echo "----------"

# Re-enable history
tmux send-keys -t $SESSION:2 "set -o history" C-m

tmux select-window -t $SESSION:2
tmux attach-session -t $SESSION
