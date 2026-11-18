#!/usr/bin/env bash
# Provisions one Teams bot for the gateway: a single-tenant Entra app, an Azure Bot resource
# pointing at the gateway, the Teams channel and (for AKS) a workload-identity federated
# credential, so no client secret ever exists.
#
#   RESOURCE_GROUP=rg-teams-a2a BOT_NAME=k8s-agent-bot \
#   ENDPOINT=https://teams-a2a.example.com/api/messages \
#   AKS_OIDC_ISSUER=$(az aks show -g rg-aks -n aks1 --query oidcIssuerProfile.issuerUrl -o tsv) \
#   K8S_NAMESPACE=teams-a2a K8S_SERVICE_ACCOUNT=gw-teams-a2a-gateway \
#   ./provision-bot.sh
#
# Without AKS_OIDC_ISSUER a client secret is created instead and printed once.
set -euo pipefail
: "${RESOURCE_GROUP:?}" "${BOT_NAME:?}" "${ENDPOINT:?}"
case "$ENDPOINT" in https://*/api/messages) ;; *) echo "ENDPOINT must be https://<host>/api/messages" >&2; exit 1 ;; esac

TENANT_ID=$(az account show --query tenantId -o tsv)

APP_ID=$(az ad app list --display-name "$BOT_NAME" --query '[0].appId' -o tsv)
if [[ -z "$APP_ID" ]]; then
  APP_ID=$(az ad app create --display-name "$BOT_NAME" --sign-in-audience AzureADMyOrg --query appId -o tsv)
  az ad sp create --id "$APP_ID" >/dev/null
fi
echo "app id: $APP_ID"

if [[ -n "${AKS_OIDC_ISSUER:-}" ]]; then
  : "${K8S_NAMESPACE:?}" "${K8S_SERVICE_ACCOUNT:?}"
  subject="system:serviceaccount:${K8S_NAMESPACE}:${K8S_SERVICE_ACCOUNT}"
  if ! az ad app federated-credential list --id "$APP_ID" --query "[?subject=='$subject']" -o tsv | grep -q .; then
    az ad app federated-credential create --id "$APP_ID" --parameters "$(cat <<JSON
{"name":"aks-${K8S_NAMESPACE}-${K8S_SERVICE_ACCOUNT}","issuer":"${AKS_OIDC_ISSUER}","subject":"${subject}","audiences":["api://AzureADTokenExchange"]}
JSON
)" >/dev/null
  fi
  echo "federated credential: $subject (use bot.auth.type: workloadIdentity)"
else
  SECRET=$(az ad app credential reset --id "$APP_ID" --display-name teams-a2a --years 1 --query password -o tsv)
  echo "client secret (store it in a secret manager now, it is not shown again): $SECRET"
fi

if ! az bot show -g "$RESOURCE_GROUP" -n "$BOT_NAME" >/dev/null 2>&1; then
  az bot create -g "$RESOURCE_GROUP" -n "$BOT_NAME" --app-type SingleTenant \
    --appid "$APP_ID" --tenant-id "$TENANT_ID" --endpoint "$ENDPOINT" --sku F0 >/dev/null
else
  az bot update -g "$RESOURCE_GROUP" -n "$BOT_NAME" --endpoint "$ENDPOINT" >/dev/null
fi
az bot msteams create -g "$RESOURCE_GROUP" -n "$BOT_NAME" >/dev/null 2>&1 || true

cat <<EOF

Done. Gateway route snippet:
  - name: ${BOT_NAME}
    bot:
      appId: "${APP_ID}"
      tenantId: "${TENANT_ID}"
      auth: {type: $([[ -n "${AKS_OIDC_ISSUER:-}" ]] && echo workloadIdentity || echo "clientSecret, clientSecret: {env: BOT_SECRET}")}
    allowedTenants: ["${TENANT_ID}"]
    allowedUsers: ["<Entra object id>"] # or allowAllTenantUsers: true (add denyGuests: true)

Next: BOT_APP_ID=${APP_ID} BOT_NAME="..." DEVELOPER_NAME="..." WEBSITE_URL=https://... deploy/teams-app/package.sh
EOF
