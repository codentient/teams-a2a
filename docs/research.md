# Research: kagent 1.0, A2A, and connecting A2A agents to Microsoft Teams

_Researched 2026-11-14. Every claim below links to the primary source it came from._

## TL;DR

- **kagent 1.0 GA'd on 2026-11-09.** It is a clean break from 0.10.x: agents now run on
  Agent Substrate (one sandboxed, suspendable Actor per conversation), the CRDs move to
  `api.kagent.dev/v1alpha3`, and **A2A is always on and speaks A2A protocol v1.0 only**.
- **There is still no standard product that attaches an arbitrary A2A agent to Teams.**
  What exists are samples, a vendor-locked low-code path (Copilot Studio / Foundry), and one
  small generic multi-chat relay. Details in [Prior art](#prior-art--what-exists-today).
- So the gateway in this repo is still worth building. kagent 1.0 adds three requirements:
  A2A v1.0 (`A2A-Version: 1.0`), the HITL extension (`https://kagent.dev/extensions/hitl/v1`),
  and a choice of caller-identity model (`insecure` / `trusted-proxy` / ServiceAccount).

## kagent 1.0: what matters for a Teams gateway

Sources: [1.0 release notes](https://kagent.dev/docs/kagent/latest/reference/release-notes/1.0/),
[Call an agent over A2A](https://kagent.dev/docs/kagent/latest/examples/a2a-agents/),
[Human in the loop](https://kagent.dev/docs/kagent/latest/agents/human-in-the-loop.md),
[Identity](https://kagent.dev/docs/kagent/latest/about/substrate-runtime/identity.md),
[Solo.io GA post](https://www.solo.io/blog/announcing-kagent-1-0-ga),
[kagent v1.0.0 changelog](https://newreleases.io/project/github/kagent-dev/kagent/release/v1.0.0).

| Topic | kagent 1.0 behaviour | What the gateway has to do |
| --- | --- | --- |
| Runtime | Each Session is an Actor on Agent Substrate. It suspends to a snapshot when idle and resumes on the next message. | Expect cold-resume latency on the first token, so show an informative "working" update right away. |
| Addressing | The controller serves A2A on port `8083`. JSON-RPC uses `POST /agents/{ns}/{name}` and the card is at `GET /agents/{ns}/{name}/.well-known/agent-card.json`. gRPC uses the `tenant` field `ns/name`. The 0.x `/api/a2a/...` paths are gone. | Accept a full card URL per route. |
| Protocol version | Every call must send `A2A-Version: 1.0`, or it fails with `requested "0.3", served "1.0"`. | Use an A2A v1.0 client (a2a-go v2 sends this header). Keep a 0.3 fallback for other agents. |
| Card URLs | `supportedInterfaces[].url` is the controller's **in-cluster** address. | Allow an endpoint override for out-of-cluster gateways. Never follow card URLs to other hosts by default (risk of SSRF or credential leak). |
| Card content | No `skills` in 1.0 cards. Capabilities include `streaming`, `pushNotifications` and `extensions` (hitl/v1, usage/v1). | Turn HITL on when the card declares it, or when config forces it (BYO-harness cards never declare it). |
| Sessions | `contextId` **is** the Session ID. If you omit it, a new Session starts. If you reuse it, the Session continues. Each turn returns a new task `id`. | Map each Teams conversation (or conversation + user) to a `contextId`. |
| Streaming | `SendStreamingMessage` emits SUBMITTED, then WORKING, then artifact chunks that share one `artifactId` (appended). | Accumulate the chunks and stream them into Teams with throttling. |
| Tool calls | Data parts carry `kagent.dev/a2a/part-type` = `function_call` / `function_response`. | Show these as informative updates ("Calling tool X…"), not in the final answer. |
| HITL | Pausing needs the `A2A-Extensions: https://kagent.dev/extensions/hitl/v1` header **on the send**. Without it, the pause can't be answered. The task goes to `INPUT_REQUIRED` with `tool_approval_request` or `ask_user_request` in `status.message.metadata[<uri>]`. The answer goes on the same `taskId` and `contextId`, with the URI in both `message.extensions` and `metadata`. | Always request the extension. Render Adaptive Cards for approve/reject and questions. Send back `tool_approval_response` / `ask_user_response`. |
| Durability | Paused tasks survive suspension and node loss. Tasks can be read later with `GetTask` / `ListTasks`. | If the stream drops, fall back to polling `GetTask`. |
| Push callbacks | Signed (Ed25519 JWT, JWKS at `/.well-known/jwks.json`) webhook per task for `INPUT_REQUIRED` and terminal states. Up to 10 retries. | Future work: proactive delivery of long-running tasks without holding a stream open. |
| Identity | `insecure` (default) trusts the caller-chosen `X-User-Id`. `trusted-proxy` decodes the bearer **without verifying it** and takes `sub` (or a configured claim). Optional ServiceAccount auth uses a projected token with audience `kagent-invocation`. Sessions are scoped to their creator, but the docs say this "is not a security boundary". | Support (a) a ServiceAccount token file, (b) a forwarded user header for trusted networks, and (c) a gateway-signed per-user JWT behind a validating proxy (for example agentgateway) in `trusted-proxy` mode. Never expose port 8083 publicly. |

Other 1.0 notes: the Agent resource is now Harness + AgentTemplate + Agent. Harnesses are `kagent`,
`codex`, `claude`, and `byo` (any image that speaks kagent's A2A contract). kagent 0.x listed Slack,
Discord, Telegram and WhatsApp "channels" (all A2A bots). The 1.0 docs ship no Teams channel and no
channel docs at all.

## Prior art — what exists today

| What | Type | Teams? | Generic A2A agent? | Gaps |
| --- | --- | --- | --- | --- |
| [Teams SDK A2A guides](https://learn.microsoft.com/en-us/microsoftteams/platform/agents-in-teams/integrate-ai/agent-to-agent) (`microsoft-teams-a2a`, `@microsoft/teams.a2a`, .NET `A2ABot`) | Samples + plugin | Yes | Partly | Built for **bot-to-bot handoff**, where each bot has its own LLM. Not a passthrough proxy. The sample has no authentication on its A2A endpoint. Uses protocol `0.3.0` cards. |
| [a2anet/microsoft-365-agents-sdk-a2a-demo](https://github.com/a2anet/microsoft-365-agents-sdk-a2a-demo) | Demo (6 commits) | Yes (M365 Agents SDK) | Yes | Echo-style forwarding only. No streaming, HITL, multi-agent routing, or production concerns. |
| [ChatAgentRelay (CAR)](https://github.com/ChatAgentRelay/ChatAgentRelay) | OSS relay (Bun/TS, ~9★) | Yes (adapter) | Yes | Generic multi-platform relay with ledger and audit. Early stage. Not focused on Teams UX (native streaming, Adaptive-Card HITL) or on kagent 1.0's HITL/identity semantics. |
| [Copilot Studio connected agents (A2A)](https://learn.microsoft.com/en-us/microsoft-copilot-studio/agents-experience/add-agent-to-agent) | SaaS, low-code | Yes (publish) | Yes, behind an orchestrator LLM | An extra LLM sits in the path. Needs licensing and a public A2A endpoint. No control over the protocol mapping. |
| [Foundry A2A endpoints + publish to Teams](https://techcommunity.microsoft.com/blog/azure-ai-foundry-blog/a2a-endpoints-and-a2a-tool-in-microsoft-foundry-agents/4557115) | PaaS | Yes | **Only Foundry agents** | Vendor lock-in. |
| [Agent Framework "channels"](https://devblogs.microsoft.com/agent-framework/introducing-agent-and-workflow-channels) (Python, Aug 2026) | Library | No Teams channel yet (Responses, Telegram, A2A, MCP) | Hosts AF agents | Not a gateway for external agents. |
| [kagent a2a-slack-template](https://github.com/kagent-dev/a2a-slack-template), [slack-a2a-deployment](https://github.com/aiagentplayground/slack-a2a-deployment) | Templates | Slack only | kagent 0.x | Use 0.x paths and protocol 0.3, so they don't work with kagent 1.0. |
| [agentgateway](https://github.com/agentgateway/agentgateway) | A2A/MCP proxy | No | Yes | Agent-to-agent proxy, not a chat-channel bridge. A good **complement** in front of kagent (JWT validation for `trusted-proxy`). |

**Conclusion:** nothing standard has appeared as of 2026-11-14. The closest generic option is CAR,
which is early and not Teams-native. Microsoft's own paths either put an LLM orchestrator in the
middle (Copilot Studio) or work only with Foundry agents.

## Teams / Bot Service protocol facts used by the gateway

Sources: [Bot Connector authentication](https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication?view=azure-bot-service-4.0),
[Teams streaming UX](https://learn.microsoft.com/en-us/microsoftteams/platform/bots/streaming-ux),
[Teams core concepts](https://learn.microsoft.com/en-us/microsoftteams/platform/teams-sdk/teams/core-concepts).

- **Inbound (Connector → bot):** a bearer JWT signed with keys from
  `https://login.botframework.com/v1/.well-known/openidconfiguration`.
  - `iss` must be `https://api.botframework.com`.
  - `aud` must be the bot App ID. Allow 5 minutes of clock skew.
  - The `serviceUrl` claim must equal `activity.serviceUrl`.
  - The signing key's `endorsements` must include `activity.channelId`.
  - Refresh the JWKS at least every 24 hours.
  - Microsoft says implementers must **not** offer a way to disable this validation.
- **Outbound (bot → Connector):** client-credentials token for scope `https://api.botframework.com/.default`
  from `login.microsoftonline.com/{tenant}` (single-tenant). Managed identity and workload identity
  are also supported. The token must only ever be sent to the Connector `serviceUrl`.
- **Streaming (1:1 chats only):** the first `typing` activity carries
  `entities[{type:"streaminfo", streamType:"informative"|"streaming", streamSequence:1}]` and returns
  `201 {id}` = streamId. Each later update is the **full text so far**, at most 1 request/second,
  with a buffer of about 1.5–2 s recommended. Close the stream with a `message` activity with
  `streamType:"final"` and no `streamSequence`.
- New bots must be single-tenant or user-assigned-MSI. Multi-tenant bot creation ended on 2025-07-31.

## A2A protocol / SDK

- A2A spec v1.0 is stable ([what's new](https://a2a-protocol.org/latest/whats-new-v1)). Methods are
  renamed (`SendMessage`, `SendStreamingMessage`, …), enums are `SCREAMING_SNAKE_CASE`, and the `kind`
  discriminators are gone.
- The Go SDK [`a2a-go/v2`](https://github.com/a2aproject/a2a-go) (v2.8.0 used here) implements v1.0
  with JSON-RPC, REST and gRPC bindings. Through `a2acompat/a2av0` it also offers 0.3 compatibility,
  so a single gateway can reach both kagent 1.0 and older 0.3 agents.
