#!/usr/bin/env bash
# Start (or stop) the Solana simnet harness.
#
#   ./run_sol_harness.sh        # start
#   ./run_sol_harness.sh stop   # stop
#
# See util/testing/sol/harness.sh for prerequisites (solana-test-validator or Docker).
set -e

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${REPO_ROOT}/util/testing/sol/harness.sh" "$@"
