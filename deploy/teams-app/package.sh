#!/usr/bin/env bash
# Builds a Teams app package (zip) for one bot. Upload it in Teams admin center or sideload it.
#   BOT_APP_ID=<app id> BOT_NAME="Kubernetes Agent" DEVELOPER_NAME="Contoso" WEBSITE_URL=https://contoso.com ./package.sh
set -euo pipefail
cd "$(dirname "$0")"
: "${BOT_APP_ID:?set BOT_APP_ID}" "${BOT_NAME:?set BOT_NAME}" "${DEVELOPER_NAME:?set DEVELOPER_NAME}" "${WEBSITE_URL:?set WEBSITE_URL}"
out=${OUT:-$PWD/${BOT_NAME// /-}.zip}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export BOT_APP_ID BOT_NAME DEVELOPER_NAME WEBSITE_URL
# shellcheck disable=SC2016 # envsubst takes a literal list of variable names
envsubst '${BOT_APP_ID} ${BOT_NAME} ${DEVELOPER_NAME} ${WEBSITE_URL}' < manifest.json > "$tmp/manifest.json"
python3 -m json.tool "$tmp/manifest.json" > /dev/null
python3 make_icons.py "$tmp"
(cd "$tmp" && zip -q -X "$out" manifest.json color.png outline.png)
echo "wrote $out"
