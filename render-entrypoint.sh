#!/bin/sh
set -eu
umask 077
: "${QODER_CONFIG_B64:?QODER_CONFIG_B64 is required}"
: "${QODER_AUTH_B64:?QODER_AUTH_B64 is required}"
mkdir -p /tmp/qoder-auth
printf '%s' "$QODER_CONFIG_B64" | base64 -d > /tmp/qoder-config.yaml
printf '%s' "$QODER_AUTH_B64" | base64 -d > /tmp/qoder-auth/qoder-cn.json
unset QODER_CONFIG_B64 QODER_AUTH_B64
exec /CLIProxyAPI/CLIProxyAPI -config /tmp/qoder-config.yaml
