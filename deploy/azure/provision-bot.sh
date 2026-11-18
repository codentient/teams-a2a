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
# Without AKS_OIDC_ISSUER a client secret is created instead and written to the Key Vault
# KEY_VAULT as SECRET_NAME (default teams-client-secret); it is never printed.
# To reuse an existing app registration, pass its APP_ID: an app is never adopted by name.
set -euo pipefail
: "${RESOURCE_GROUP:?}" "${BOT_NAME:?}" "${ENDPOINT:?}"
case "$ENDPOINT" in https://*/api/messages) ;; *) echo "ENDPOINT must be https://<host>/api/messages" >&2; exit 1 ;; esac

TENANT_ID=$(az account show --query tenantId -o tsv)

if [[ -z "${AKS_OIDC_ISSUER:-}" ]]; then
  : "${KEY_VAULT:?KEY_VAULT is required: the client secret is written there, never printed}"
fi
SECRET_NAME=${SECRET_NAME:-teams-client-secret}

if [[ -n "${APP_ID:-}" ]]; then
  az ad app show --id "$APP_ID" --query appId -o tsv >/dev/null
else
  # Display names are not unique: never adopt someone else's app with the same name.
  existing=$(az ad app list --display-name "$BOT_NAME" --query 'length(@)' -o tsv)
  if [[ "$existing" != 0 ]]; then
    echo "An app registration named '$BOT_NAME' already exists; pass APP_ID=<app id> to reuse it explicitly." >&2
    exit 1
  fi
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
  az keyvault secret set --vault-name "$KEY_VAULT" --name "$SECRET_NAME" --file <(printf '%s' "$SECRET") --output none
  unset SECRET
  echo "client secret written to Key Vault $KEY_VAULT as $SECRET_NAME (mount it with the chart's keyVault.secrets)"
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
      auth: {type: $([[ -n "${AKS_OIDC_ISSUER:-}" ]] && echo workloadIdentity || echo "clientSecret, clientSecret: {file: /mnt/secrets-store/${SECRET_NAME}}")}
    allowedTenants: ["${TENANT_ID}"]
    allowedUsers: ["<Entra object id>"] # or allowAllTenantUsers: true (add denyGuests: true)

Next: BOT_APP_ID=${APP_ID} BOT_NAME="..." DEVELOPER_NAME="..." WEBSITE_URL=https://... deploy/teams-app/package.sh
EOF
