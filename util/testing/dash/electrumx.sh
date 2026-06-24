#!/usr/bin/env bash
#
# Set up a Dash regtest electrumx server for the electrum-backed wallet tests.
# Delegates to the shared btc/electrumx.sh (upstream spesmilo electrumx, which
# ships a DashRegtest coin class). Arguments: COIN, dashd RPC port, electrum SSL
# port.
#   - dashd RPC port 20777 = util/testing/dash/harness.sh ALPHA_RPC_PORT
#   - electrum SSL port 55021 = assets/dash.go dashSimnetElectrumServers
#
# Prerequisite: the Dash regtest harness must be running (./harness.sh), and
# electrumx needs the x11_hash python package (btc/electrumx.sh installs it when
# COIN=Dash). Serves ssl://127.0.0.1:55021.

../btc/electrumx.sh Dash 20777 55021
