#!/usr/bin/env bash
# tmux script that sets up a BNB simnet harness. There is only one node in
# --dev mode.
set -ex

export CHAIN="bnb"

export ALPHA_AUTHRPC_PORT="8554"
export ALPHA_HTTP_PORT="48556"
export ALPHA_WS_PORT="48557"
export DEV_PERIOD="1"

export NODES_ROOT=~/dextest/bnb

../eth/baseharness.sh
