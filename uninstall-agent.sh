#!/bin/sh
set -eu

echo "==> uninstall probe-agent"

if command -v systemctl >/dev/null 2>&1; then
  systemctl stop probe-agent 2>/dev/null || true
  systemctl disable probe-agent 2>/dev/null || true
fi
if command -v rc-service >/dev/null 2>&1; then
  rc-service probe-agent stop 2>/dev/null || true
  rc-update del probe-agent default 2>/dev/null || true
fi

rm -f /etc/systemd/system/probe-agent.service
rm -f /etc/init.d/probe-agent /etc/conf.d/probe-agent /run/probe-agent.pid
rm -f /etc/probe/probe-agent
rm -rf /etc/probe

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl reset-failed
fi

echo "Uninstall done."
