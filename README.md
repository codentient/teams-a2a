# teams-a2a-gateway

A secure, fast gateway that puts **any A2A agent into Microsoft Teams** through Azure Bot Service.
It is built and tested against **kagent 1.0** (A2A protocol v1.0, human-in-the-loop extension).
It also works with any other A2A v1.0 or v0.3 agent.

```
Teams ──► Azure Bot Service ──► teams-a2a-gateway ──A2A──► kagent / ADK / LangGraph / Foundry / …
```

As of November 2026 there is still no standard product for this. Microsoft ships bot-to-bot A2A
samples, and its products either put an LLM orchestrator in the path (Copilot Studio) or only
accept their own agents (Foundry). kagent 1.0 has no Teams channel. See
[docs/research.md](docs/research.md).

## What you get

- **Zero bot code per agent.** One config entry maps an Azure Bot (app id) to an A2A agent card.
  One gateway can serve many bots.
- **Native Teams UX:**
  - Streaming in 1:1 chats.
  - "Running `tool`…" progress from kagent tool calls.
  - Threaded replies in channels and @mention handling.
  - AI-generated label on answers.
  - Agent-supplied images (PNG, JPEG, GIF, WebP) and, when a route enables them, sanitised
    Adaptive Cards.
  - `/new`, `/cancel` and `/help` commands.
- **Human in the loop:** kagent `tool_approval_request` and `ask_user_request` pauses become
  Adaptive Cards (Approve/Reject with a reason, multiple choice, free text). Tool calls are
  approved only with the card's Approve button; typing `reject` works, but no typed word
  approves. Approvals expire after `approvalTTL` (30 minutes) and are then rejected. Questions
  can be answered in plain text. Only the requester can answer, and cards are retired once used.
- **Security:**
  - Full Bot Connector JWT validation (issuer, audience, expiry, signature, serviceUrl claim,
    channel endorsement).
  - Tenant and user allowlists. The bot token only goes to allowlisted Connector hosts.
  - Agent credentials are pinned to the card's origin.
  - Per-user rate limits.
  - Workload identity, managed identity, a projected kagent ServiceAccount token, or per-user
    signed JWTs. No secrets in config.
  - Distroless non-root image.
- **Resilience:**
  - Instant ack to Bot Service.
  - Redis-backed sessions, locks and dedupe for multiple replicas.
  - Falls back to polling `GetTask` if a stream drops.
  - Turn timeouts cancel the task.
  - Graceful shutdown, and startup that survives an IdP blip.
- **Observability:** Prometheus metrics (time to first output, turn outcomes, Connector latency,
  auth failures), JSON logs, and health/readiness on a private admin port.

Design, threat model and roadmap: [docs/design.md](docs/design.md).

## Try it locally (no Azure needed)

The repo ships a kagent-like demo agent and a Bot Service simulator. The simulator signs real
Bot Connector tokens and records what the gateway sends back.

```bash
go run ./cmd/demo-agent &                              # A2A v1.0 agent on :8083
go run ./cmd/teams-sim "hello" "delete pod web-1" approve "scale it" staging &
cat > /tmp/gw.yaml <<'EOF'
server: {listen: "127.0.0.1:3978", adminListen: "127.0.0.1:9090"}
teams:
  openIdMetadataURL: http://127.0.0.1:4000/v1/.well-known/openidconfiguration
  loginAuthority: http://127.0.0.1:4000
  serviceUrlAllowlist: ["127.0.0.1"]
  allowInsecureServiceURLs: true
routes:
  - name: demo
    bot: {appId: "00000000-0000-0000-0000-000000000001", tenantId: "00000000-0000-0000-0000-0000000000aa",
          auth: {type: clientSecret, clientSecret: {value: dev}}}
    allowedTenants: ["00000000-0000-0000-0000-0000000000aa"]
    agent: {cardURL: http://127.0.0.1:8083/agents/kagent/demo/.well-known/agent-card.json}
EOF
go run ./cmd/teams-a2a-gateway -config /tmp/gw.yaml
```

To test the image and Helm chart in a throwaway kind cluster, run `deploy/kind/smoke.sh`.

## Deploy with kagent 1.0 on AKS

1. **kagent:** enable ServiceAccount callers (`config.server.auth.serviceAccount.authority`) and
   keep port 8083 cluster-internal.
2. **Bot:** run `deploy/azure/provision-bot.sh`. It creates a single-tenant app, a workload-identity
   federated credential, the Azure Bot and the Teams channel, and prints the route snippet.
3. **Gateway:** use `helm install` with `deploy/helm/teams-a2a-gateway`. Start from
   `deploy/helm/teams-a2a-gateway/ci/kagent-values.yaml`:
   - set `workloadIdentity.enabled` and `kagentToken.enabled`;
   - use `store.type: redis` for more than one replica;
   - expose only `/api/messages` through the Ingress.
   - with a client secret instead of workload identity, keep it in a Secret and mount it with
     `secretMounts: [{name: teams-bot, secretName: <secret>}]`, then set
     `clientSecret: {file: /var/run/secrets/teams-a2a/teams-bot/clientSecret}`. The file is
     re-read, so rotation needs no restart.
4. **Teams app:** run `deploy/teams-app/package.sh`, then upload the zip in the Teams admin center.

Example configs:

- [examples/kagent.yaml](examples/kagent.yaml): ServiceAccount identity.
- [examples/kagent-user-identity.yaml](examples/kagent-user-identity.yaml): per-user JWT through
  agentgateway, so kagent Sessions are owned by the real Teams user.
- [examples/multi-agent.yaml](examples/multi-agent.yaml): several bots, OAuth2 agents, managed
  identity.

## Configuration reference (route)

| Key | Default | Notes |
| --- | --- | --- |
| `bot.appId`, `bot.tenantId` | required | The Azure Bot / Entra app. Routing uses the validated token audience. |
| `bot.auth.type` | `clientSecret` | Also `workloadIdentity` (AKS) or `managedIdentity`. |
| `allowedTenants` | required | Activities from other tenants are dropped. |
| `allowedUsers` | all | Optional list of Entra object ids. |
| `sessionScope` | `conversation` | `user` gives each person their own agent session in group chats and channels. |
| `approvalPolicy` | `requester` | Only the person who started a request can answer its prompts, `/cancel` it, or discard it with `/new` (until it is older than `approvalTTL`). `anyone` lets any chat member do so. |
| `approvalTTL` | `30m` | A tool approval not answered in time is rejected (1m to 24h). |
| `streaming`, `streamInterval` | `true`, `1.5s` | Native streaming in 1:1 chats. The interval must be at least 1s (Teams throttle). |
| `showToolActivity`, `aiLabel` | `true` | |
| `turnTimeout` | `10m` | The task is cancelled when the timeout is hit. |
| `rateLimit` | 20/min, burst 5 | Per user. |
| `welcome` | none | Message posted when the bot is added. |
| `agent.cardURL` | required | kagent: `http://kagent-controller.kagent.svc:8083/agents/<ns>/<name>/.well-known/agent-card.json`. Redirects are never followed: use the final URL. |
| `agent.endpoint` | from card | Override for in-cluster card URLs or proxies. Fields: `url`, `binding` (JSONRPC / HTTP+JSON), `protocolVersion` (1.0 / 0.3). |
| `agent.auth.type` | `none` | Also `bearer`, `tokenFile`, `oauth2ClientCredentials` or `userJWT`. |
| `agent.identityHeaders` | none | For example `{X-User-Id: aadObjectId}`. Only for trusted networks. |
| `agent.hitl` | `auto` | `always` for BYO-harness agents, whose cards don't declare the extension. |
| `agent.cards` | off | `{enabled: true, allowedHosts: [docs.example.com, "*.example.org"]}` shows agent-supplied Adaptive Cards after sanitising: display elements only, images from `data:` URLs or allowed hosts, `Action.OpenUrl` to allowed hosts, `Action.ShowCard`; no buttons that send data or invoke the bot. Agent output is untrusted, so keep it off unless the agent needs it. |

Server-level keys (`server`, `store`, `teams`) are documented inline in
[internal/config/config.go](internal/config/config.go).

## Development

```bash
make test        # go vet + race tests (unit + end-to-end against fake Teams and a kagent-like agent)
make image       # distroless image
make smoke       # kind cluster smoke test of image + Helm chart
```

## Images

CI publishes `ghcr.io/codentient/teams-a2a-gateway` (linux/amd64 and linux/arm64) only after
the tests and the kind smoke test pass:

- every push to `main` as `sha-<commit>` and `main`;
- every `v*` tag as `<version>`, for example `v0.2.0` as `0.2.0`.

Tags move; deploy by digest. The digest is in the run summary. Each image has a GitHub build
provenance attestation:

```bash
gh attestation verify oci://ghcr.io/codentient/teams-a2a-gateway@sha256:<digest> --repo codentient/teams-a2a
```
