#!/usr/bin/env bash
set -euo pipefail

echo "==> uninstall probe-agent"

systemctl stop probe-agent 2>/dev/null || true
systemctl disable probe-agent 2>/dev/null || true

rm -f /etc/systemd/system/probe-agent.service
rm -f /etc/probe/probe-agent
rm -rf /etc/probe

systemctl daemon-reload
systemctl reset-failed

echo "Uninstall done."
