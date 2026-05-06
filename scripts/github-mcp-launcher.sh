#!/bin/bash
# Wrapper for launching github-mcp-server with the PAT pulled from a local
# .env file. Used by .mcp.github-direct.json so the token never appears on
# the command line or in the MCP config JSON.
#
# Resolution order for the token:
#   1. GITHUB_PERSONAL_ACCESS_TOKEN already in env → use as-is.
#   2. INTENT_GW_ENV_FILE (or ./.env)            → read SLEUTH_TEST_GITHUB_TOKEN
#                                                  or GITHUB_PERSONAL_ACCESS_TOKEN
#                                                  from that file.
set -eu

if [ -z "${GITHUB_PERSONAL_ACCESS_TOKEN:-}" ]; then
  ENV_FILE="${INTENT_GW_ENV_FILE:-$(dirname "$0")/../.env}"
  if [ -f "$ENV_FILE" ]; then
    for key in GITHUB_PERSONAL_ACCESS_TOKEN SLEUTH_TEST_GITHUB_TOKEN; do
      val=$(grep -E "^${key}=" "$ENV_FILE" | head -1 | cut -d= -f2- | sed 's/^["'\'']//; s/["'\'']$//')
      if [ -n "$val" ]; then
        export GITHUB_PERSONAL_ACCESS_TOKEN="$val"
        break
      fi
    done
  fi
fi

if [ -z "${GITHUB_PERSONAL_ACCESS_TOKEN:-}" ]; then
  echo "github-mcp-launcher: no GitHub PAT found (env or .env file)" >&2
  exit 1
fi

exec github-mcp-server stdio
