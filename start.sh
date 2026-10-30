#!/usr/bin/env bash
# Exports the settings in .env (copy .env.example) and runs the server with
# go run. Override the file with ENV_FILE.
set -euo pipefail
cd "$(dirname "$0")"

ENV_FILE="${ENV_FILE:-.env}"
[[ -f "$ENV_FILE" ]] || { echo "start.sh: $ENV_FILE not found; copy .env.example" >&2; exit 1; }

# Lines are parsed rather than sourced, so an unquoted mnemonic with spaces
# stays one value. A matching pair of surrounding quotes is stripped.
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line%$'\r'}"
  [[ -z "$line" || "$line" == \#* ]] && continue
  key="${line%%=*}"
  value="${line#*=}"
  if [[ ${#value} -ge 2 && ( "$value" == \"*\" || "$value" == \'*\' ) ]]; then
    value="${value:1:${#value}-2}"
  fi
  export "$key=$value"
done <"$ENV_FILE"

exec go run .
