# Design

## Goals

1. Let **any A2A-capable agent** answer in Microsoft Teams without writing bot code: one config
   entry per agent. Supported agents include kagent 1.0, Google ADK, LangGraph, CrewAI, Foundry,
   and any other A2A v1.0 or v0.3 server.
2. **Secure by default.** Inbound tokens are fully validated. Credentials only go to pinned
   destinations. Every route needs a tenant allowlist. Human-in-the-loop prompts are bound to the
   person who started the request.
3. **Fast.** The gateway answers Bot Service immediately and streams the answer as it is produced.
   It is a single 36 MB static binary with pooled connections and cached tokens and cards.
4. **Channel-agnostic core**, so Teams via Azure Bot Service is the first channel, not the only
   one.

## Architecture

```
 Teams client ──► Teams backend ──► Azure Bot Service ──HTTPS──►  /api/messages
                                                                 │
                       ┌──────────────── teams-a2a-gateway ─────┴──────────────────────┐
                       │ teams.Validator   JWT: iss, aud=bot appId, exp±5m, RS256,     │
                       │                   serviceUrl claim, channel endorsement       │
                       │ gateway.Server    route by validated aud · tenant/user allow- │
                       │                   lists · dedupe · rate limit · 200 at once   │
                       │ bridge.Bridge     channel-neutral turn engine: session ↔ A2A  │
                       │                   contextId, HITL state, polling fallback      │
                       │ agent.Agent       A2A client (a2a-go v2): card cache, origin   │
                       │                   pinning, credentials, identity, extensions  │
                       │ teams.Sink        paced native streaming, Adaptive Cards       │
                       │ store             memory | Redis (sessions, locks, dedupe)     │
                       └───────────┬──────────────────────────────┬─────────────────────┘
                                   │ A2A v1.0 (JSON-RPC/REST, SSE) │ Bot Connector REST
                                   ▼                               ▼
                        kagent controller :8083           smba.trafficmanager.net
                        /agents/{ns}/{name}               (token: client secret,
                        or any A2A agent                   workload identity or MSI)
```

### One turn

1. Bot Service POSTs an activity. The gateway validates the token before it acts on anything,
   picks the route by the **validated** `aud`, checks the tenant and user allowlists, drops
   redeliveries, rate-limits the user, and returns `200` right away.
2. In the background it takes a per-chat lock (memory or Redis) and loads the session: the A2A
   `contextId` plus any pending input request. It then builds the A2A message:
   - a new message, with the chat's `contextId`, or
   - a continuation of a paused task (`taskId` + `contextId`), or
   - a kagent HITL response (`tool_approval_response` / `ask_user_response`) with the extension
     declared in `extensions` and `metadata`, exactly as kagent's `go/api/a2a/hitl.go` expects.
3. It calls `SendStreamingMessage` (or `SendMessage` when the card has no streaming). Headers:
   `A2A-Version: 1.0`, `A2A-Extensions: https://kagent.dev/extensions/hitl/v1` when enabled, the
   route's credential, and optional identity headers.
4. Events are folded by an accumulator:
   - Artifact chunks with the same id are appended.
   - A non-append update replaces the artifact.
   - Tool calls (`kagent.dev/a2a/part-type`) and thoughts become informative updates and never
     appear in the answer.
5. The Teams sink streams:
   - It sends informative updates, then the full text so far, at most once per `streamInterval`
     (Teams allows one update per second).
   - It closes with a `final` message that carries the AI label.
   - Group chats and channels don't support streaming, so there the sink sends typing indicators
     instead.
   - If Teams rejects streaming, the sink falls back to a single plain message.
6. Outcomes:
   - `INPUT_REQUIRED` with a HITL payload: an Adaptive Card. Without one: the question as text,
     and the next message continues the task.
   - `FAILED` / `REJECTED` / `CANCELED`: explained to the user.
   - Timeout: the gateway cancels the task.
   - If the stream drops after the task id is known, the gateway polls `GetTask`. kagent persists
     tasks, so the answer is not lost.

## Security model

| Threat | Control |
| --- | --- |
| Forged requests to `/api/messages` | Full Bot Connector JWT validation: RS256 only; `iss` = `https://api.botframework.com`; `aud` is one of the configured app ids; `exp`/`nbf` within ±5 minutes; `serviceurl` claim equals the activity; the signing key is endorsed for the channel. No switch to disable it (Microsoft requires this). JWKS refreshed every 12 hours, and on an unknown `kid` at most once per 5 minutes. |
| Wrong bot / confused deputy | Route chosen from the validated `aud`. `recipient.id` must match. |
| Other tenants installing a multi-tenant bot | `allowedTenants` is required. Activities from other tenants are dropped. |
| Bot token exfiltration | Outbound bot tokens only go to `serviceUrl`s on an allowlist: `smba.trafficmanager.net`, `*.botframework.com`, GCC/GCCH hosts. It is not `*.trafficmanager.net`, which anyone can register. https is required; credentials, query strings and fragments are refused. |
| Agent credential exfiltration through a tampered card | Interfaces must be on the **card's origin**, unless an explicit `endpoint` override or `allowCrossOriginInterfaces` is set. |
| HITL hijack: someone else approves, or a forged or replayed card | Each card carries a random nonce stored server-side. A submission must match the pending nonce. `approvalPolicy: requester` (the default) checks the clicker's Entra id. Answered cards are replaced with a summary. Choice answers must be valid indexes, and free text is matched against the offered choices. Card submissions can only address sessions inside the chat they were clicked in. Tool calls are approved only by the card (bound to the nonce): typed words can reject but never approve, so a casual "ok" or a button that makes the user send text cannot run a tool. Approvals expire after `approvalTTL` (default 30m); any later answer rejects the request, tells the agent why and retires the card. With `approvalPolicy: requester`, `/cancel` works only for the user who started the running task, and `/new` is refused while another user's request is open (for up to `approvalTTL`), so a chat member cannot wipe or kill someone else's work. |
| Over-broad access to privileged agents | Every route must choose who may use it: `allowedUsers`, or an explicit `allowAllTenantUsers: true`; there is no implicit "whole tenant". `denyGuests` refuses B2B guests (whose activities carry the resource tenant id) after a cached Bot Connector roster lookup, failing closed. Group-based access is delegated to Teams app permission policies on the bot's Teams app. |
| Identity spoofing towards the agent | Identity headers are always overwritten, or removed, from validated activity data. A static `Authorization` header is rejected in config. Prefer `userJWT` behind a validating proxy, or the ServiceAccount token, over plain headers. |
| Duplicate execution | Activity id dedupe (Redis `SETNX`, 30 min). A per-chat turn lock means one turn at a time; others get "still working, /cancel". |
| Abuse / cost blow-up | Per-user token bucket. Global concurrency cap (`maxConcurrentTurns`). Body size limit. Per-turn timeout with task cancellation. |
| Prompt-injected agent output | Agent cards are off by default. When enabled, an allowlist sanitiser keeps display elements, `data:` or allowlisted images, allowlisted `Action.OpenUrl` and `Action.ShowCard`, and drops `Action.Submit`/`Execute`, `msteams` (`imBack`, `messageBack`, `invoke`, `signin`), `selectAction`, `backgroundImage`, `refresh` and inputs, with size, depth and element limits. This blocks zero-click exfiltration through image URLs, forged approve buttons and phishing buttons. Raw images must be PNG, JPEG, GIF or WebP and match their bytes; SVG is never shown. |
| Credential replay through redirects | No outbound client follows redirects: agent calls, OAuth2 token requests, bot token requests, Bot Connector calls and signing-key fetches. A 307/308 would otherwise replay the body, and on the same host with another port the `Authorization` header, to an origin that pinning and allowlists never approved. A redirect is reported as an error. |
| Untrusted traffic driving work, weak trust anchors | Signing keys are refreshed by a background loop (every ~12h with jitter, backoff on failure); a request never triggers a fetch for a known key, and an unknown key id at most once per 5 minutes, so unauthenticated traffic cannot drive outbound calls. Requests without a bearer token are refused before the body is read; the body limit is 256 KiB; rejection logs are sampled. The OpenID metadata, login authority and OAuth2 token URLs must be https (only the test flag `allowInsecureServiceURLs` relaxes the Teams ones), inline secrets need `server.allowInlineSecrets`, hop-by-hop, cookie and forwarding headers cannot be configured, and allowlist wildcards need two labels (`*.botframework.com`, not `*.net`). |
| Forensics and disclosure | Every human decision that changes what an agent does (tool approval, rejection, expiry, `ask_user` answer, `/cancel`, `/new`) is logged once as `msg=audit audit=true` with the event, decision, user id, name and tenant, task and context ids, tools and reason; ship these to tamper-resistant storage. Agent failure text (which can carry stack traces or internal hosts) is logged under a reference; the user sees only the reference. `userJWT` requires `sessionScope: user`, so identities are never mixed in one agent context. |
| Rollouts | On SIGTERM the gateway reports not-ready, keeps serving for `server.drainDelay` (5s) while load balancers drop it, then stops accepting and drains turns for `server.shutdownTimeout` (30s). Agent turns still running after that are cancelled, so keep `terminationGracePeriodSeconds` above drainDelay + shutdownTimeout. |
| Resource exhaustion by one user or agent | A user may run at most `server.maxTurnsPerUser` (3) turns at once across chats, so nobody holds most of `maxConcurrentTurns`. Streamed answers cost linear time (chunks are appended to a builder; partial text is rendered at most every 200 ms) and are capped at 1 MiB: beyond that the task is cancelled and the user is told. |
| Supply chain and provisioning | `golang.org/x/net` bumped past GO-2026-6866/6860 (`govulncheck` clean); base images pinned by digest, Actions by SHA, `govulncheck` by version, kept current by Dependabot; images carry GitHub provenance and SBOM attestations. Signing keys get the same RSA checks in both JWK forms (≥ 2048 bits, odd exponent ≥ 3). The Teams manifest requests only `identity`; `provision-bot.sh` never adopts an app by display name (pass `APP_ID`) and writes a client secret to Key Vault instead of printing it. |
| Secrets at rest | Secrets come from env or files, never from config literals in production. Supports workload identity (no secret) and managed identity. File secrets are re-read, so they rotate without a restart. |
| Container | Distroless static image, UID 65532, read-only root fs, all capabilities dropped, seccomp `RuntimeDefault`. The SA token is not automounted; only an audience-scoped projected token is mounted. |
| Admin surface | `/metrics`, `/readyz` and the JWKS are on a separate listener and Service. The NetworkPolicy limits it to chosen namespaces. Only `/api/messages` is in the Ingress. |

### Identity choices for kagent 1.0

kagent's own docs say that `insecure` lets callers pick their principal, and that `trusted-proxy`
doesn't verify the token. So the gateway offers three postures:

1. **ServiceAccount (recommended to start):** `agent.auth.type: tokenFile` with a projected token
   whose audience is `kagent-invocation`. kagent verifies it with a TokenReview. All Sessions belong
   to the gateway's identity, and the gateway keeps users apart (`sessionScope: user`).
2. **Per-user via a validating proxy:** `agent.auth.type: userJWT`. The gateway signs a 5-minute
   ES256 JWT per Teams user (`sub` = Entra object id) and publishes its JWKS on the admin port.
   agentgateway or another proxy validates it. kagent then runs in `trusted-proxy` mode, and its
   Sessions are owned by the real user.
3. **Forwarded header (lab only):** `identityHeaders: {X-User-Id: aadObjectId}` with kagent
   `insecure`. Only acceptable when nothing but the gateway can reach port 8083.

## Performance notes

- Bot Service gets `200` before any agent work starts, so its 15-second budget is never at risk.
- HTTP/2 and keep-alive pools are shared. Bot tokens, agent tokens, user JWTs and agent cards are
  cached. Concurrent token refreshes collapse into one.
- Streaming updates are coalesced: the A2A event loop never waits on Teams.
- kagent Actors resume from snapshots on the first message, so an informative update goes out
  immediately to cover cold-resume latency.
- Metrics: `teams_a2a_time_to_first_output_seconds`, `teams_a2a_turn_duration_seconds`,
  `teams_a2a_connector_request_seconds`, `teams_a2a_turns_total{outcome}`,
  `teams_a2a_auth_failures_total`.

## Extending to other channels ("other means")

`bridge` knows nothing about Teams. A channel adapter has three jobs:

1. Authenticate inbound traffic and resolve an `identity.User`.
2. Build a `bridge.Turn` with a session key.
3. Implement `bridge.Sink`: `Progress`, `Partial`, `Final`, `Notice`, `Prompt`, `Retire`.

Candidate channels:

| Channel | Status / notes |
| --- | --- |
| Teams via Azure Bot Service | **Implemented.** |
| Microsoft 365 Copilot (custom engine agent) | Uses the same Bot Service activity protocol. Probably a matter of adding its channel id to `teams.allowedChannels` and checking rendering (streaming, cards). Not verified yet. |
| Teams outgoing webhooks / Graph RSC listeners | Possible for "listen in channel without @mention" scenarios. Needs Graph change notifications. |
| Slack, Discord, Telegram | Same pattern as kagent's own A2A bots. Only a sink and an authenticator are needed. |
| Proactive / long-running delivery | Use kagent's signed push callbacks (`CreateTaskPushNotificationConfig`, Ed25519 JWT + JWKS) to post results to Teams after the stream has closed, instead of holding the turn open. |

## Known limitations / next steps

- Inbound files and images from Teams are not forwarded to the agent yet (the user gets a
  notice). Forwarding is opt-in work, because Teams download URLs grant access to the file.
- Tool approval is all-or-nothing per request. kagent supports per-tool decisions, which could be
  added as toggles on the card.
- Rate limiting is per replica. Use the agent platform's quotas for global limits.
- Teams SSO / OBO to pass the user's own Entra token to the agent (for agents that call Graph as
  the user) is not implemented.
- Not yet tested against a live Teams tenant or a live kagent 1.0 cluster from this repo. The
  protocol details come from the official docs and from kagent's source (commit `eb42f64`).
  Validation used a protocol-faithful fake of both sides plus a kind cluster.
