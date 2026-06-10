#!/usr/bin/env bash
set -euo pipefail

if [[ -f /config/rippled.cfg ]]; then
    cp /config/rippled.cfg /etc/opt/ripple/rippled.cfg
fi
if [[ -f /config/validators.txt ]]; then
    cp /config/validators.txt /etc/opt/ripple/validators.txt
fi
exec rippled --conf /etc/opt/ripple/rippled.cfg "$@"
