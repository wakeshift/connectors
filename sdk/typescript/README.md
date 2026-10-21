# @wakeshift/sdk

Let agents subscribe to things in your app. Inside one of your MCP tools you subscribe the conversation that called it to a topic; when you publish to that topic, Wakeshift wakes the agent. You don't deal with harnesses: Wakeshift puts what each one needs (Claude Code, Codex, and the rest) into your tool's result.

Node 22 or later. Peer dependencies: `@modelcontextprotocol/sdk` (1.23 or later: the first that takes zod 4) and `zod` (3.25 or later, or 4). Import `z` from `"zod"`, not `"zod/v4"`: `ws.args` is built from `"zod"`, and the MCP SDK refuses a schema that mixes the two.

## Install

The SDK isn't on npm yet. Install it from the tarball that came with your invite, or pack one from this directory:

```sh
# in sdk/typescript
npm install && npm pack          # writes wakeshift-sdk-0.1.0.tgz

# in your app
npm install /path/to/wakeshift-sdk-0.1.0.tgz @modelcontextprotocol/sdk zod
```

Use the tarball rather than `npm install /path/to/sdk/typescript` (a `file:` dependency). A `file:` dependency links the directory along with its `node_modules`, so the SDK can load its own development copy of zod instead of yours, and the MCP SDK then fails with "Mixed Zod versions detected in object shape". `scripts/pack-check.sh` installs the tarball with zod 3 and with zod 4, each with the oldest MCP SDK it supports and the latest, to check it.

## Quickstart: a CI bot

You have an MCP server and a GitHub webhook handler. Register an app in the Wakeshift console (Build), and put its key in `WAKESHIFT_KEY` and the console's address in `WAKESHIFT_CORE` (for example `https://app.staging.wakeshift.ai`). Load them however you load environment variables, for example `node --env-file=.env server.ts`.

```ts
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const ws = new Wakeshift({ key: process.env.WAKESHIFT_KEY!, core: process.env.WAKESHIFT_CORE })
const server = new McpServer({ name: "ci-bot", version: "1.0.0" })

server.registerTool(
  "watch_ci",
  {
    description: "Wake me when CI finishes on a repo",
    inputSchema: { repo: z.string(), branch: z.string().optional(), ...ws.args },
  },
  async ({ repo, branch, ...args }, extra) => {
    if (!(await canSeeRepo(extra, repo))) throw new Error(`You can't see ${repo}.`)
    const user = extra.authInfo?.extra?.userId as string | undefined // your own ID for the person, if you have one
    const sub = await ws.subscribe(ws.call(extra, { server, user, args }), `ci/${repo.toLowerCase()}`, {
      filter: branch ? { branch } : undefined,
      replay: "5m",
    })
    return sub.result(`Watching CI on ${repo}.`)
  },
)
```

```ts
// In your GitHub webhook handler, after checking X-Hub-Signature-256:
if (req.headers["x-github-event"] === "workflow_run" && body.action === "completed") {
  const run = body.workflow_run
  await ws.publish(`ci/${body.repository.full_name.toLowerCase()}`, {
    id: String(req.headers["x-github-delivery"]),
    text: `CI ${run.conclusion} on ${run.head_branch}: ${run.name}`,
    data: { branch: run.head_branch, url: run.html_url },
  })
}
```

That's all. An agent that calls `watch_ci` is woken with the text and data of each publish that passes its filter, framed as information from your app, not as instructions from its user.

- `...ws.args` adds three optional arguments to this tool, and the SDK touches no other tool or result:
  - `session_key`: Claude Code sends no conversation ID, so the result gives the agent a key to pass back; without it, a second call makes a second session. Keys are random (`wsc_…`), so one conversation can't guess another's, but a key is a bearer credential for its conversation (its description tells the agent so): don't log tool arguments or results that carry it. A key Wakeshift doesn't know (it ended, or the agent mistyped it) starts a new session on `subscribe`, and the result says so; other calls ignore it.
  - `harness`: the program the agent runs in. Some programs' calls look alike (Grok Bot's and Cursor's cloud agents' are Cursor's), so the agent says which it is and gets instructions that work there. It's a hint: it only refines what the call was recognised as, and one that contradicts the client name is ignored.
  - `conversation_id`: the agent's own ID for the conversation, where its program gives one (Cursor and Grok Bot: `CURSOR_CONVERSATION_ID`).
- `ws.call` reads them from `args`, and the client's name from `server`.
- For an agent that can't be woken, messages are held, and the result tells it to call the tool again to check for them. To show them in another tool's results too, spread `...ws.args` into that tool's input and return `ws.annotate(ws.call(extra, { server, args }), result)`.
- Wakeshift tells harnesses apart by the MCP client's name, which the client sends in `initialize`. A server created per request (the stateless Streamable HTTP pattern) never sees `initialize`, so Wakeshift falls back to the tool call's `_meta` and the request's User-Agent, which `ws.call` forwards. That covers Claude Code, Codex, Antigravity, Copilot CLI, opencode and Cursor. For any other client on a stateless server, keep one `McpServer` per MCP session, or pass the name yourself with `ws.call(extra, { client })`. The SDK warns once when Wakeshift recognised nothing and the agent didn't say, and `sub.agent` says what it recognised.
- Your test key works against test workspaces only, which Wakeshift makes for your users. Nothing else changes when live keys arrive.

## Reference

### `new Wakeshift({ key, core?, fetch?, timeoutMs?, onWarning? })`

- `key`: `wst_test_<app>_<seed>`. It's your app's Ed25519 private key; it never goes over the wire. Each request carries a fresh assertion signed with it (valid two minutes).
- `core`: the address of the console the key came from, for example `https://app.staging.wakeshift.ai`. Default: `WAKESHIFT_CORE` from the environment. A test key needs one of the two, and the constructor throws without them; a live key falls back to `https://app.wakeshift.ai`.
- `onWarning(message)`: called once per kind of problem that doesn't fail a call, such as a tool call Wakeshift recognised no harness in. Default: `console.warn`.
- `ws.appId`, `ws.mode` (`"test"` or `"live"`), `ws.core`.

### The caller

- `ws.call(extra, { server, user?, args?, session?, harness?, conversation?, client? })` packs a tool call from an MCP tool callback's `extra`: its `_meta` (Codex sends its thread there), the request's User-Agent (from `extra.requestInfo`), the client's name (from `server`'s client info; or pass `client` yourself, for example on a stateless server), `user` (your ID for the person; their conversations are grouped, and `u/<user>/…` topics are theirs), and `session_key`, `harness` and `conversation_id` from `args`.
- `ws.callOf({ meta, clientName, user?, session?, userAgent?, harness?, conversation? })` does the same without an MCP framework.
- `ws.args`: a zod shape with `session_key`, `harness` and `conversation_id` (`SESSION_ARG`, `HARNESS_ARG`, `CONVERSATION_ARG`), to spread into your subscribe tool's `inputSchema` (and any tool whose results you annotate).

### Topics

Lowercase segments of `a-z 0-9 . _ -` joined by `/`, up to 200 characters, scoped to your app. `u/<user>/…` is private: Wakeshift refuses to subscribe a caller whose `user` isn't `<user>`. So a user with private topics needs an ID that is itself a topic segment (lowercase `a-z 0-9 . _ -`): use an opaque ID, or a hash of yours, not an email. Every other topic is shared, so check access before `subscribe`, and when someone loses access, end their subscriptions with `unsubscribeUser` (or `endSubscription`). Don't put secrets in topic names.

### Calls

| Call | Returns |
| --- | --- |
| `subscribe(call, topic, { filter?, once?, replay?, ttl? })` | `{ id, topic, filter, once, expiresAt, session, addition, agent, result(textOrResult) }`. `agent`: `{ harness, via, claimed }`, what Wakeshift recognised the caller as and by which signal (`"client"`, `"meta"`, `"user_agent"`, or `"claim"` when only the agent's `harness` named it), both `null` when nothing; `claimed` is what the agent said (normalized), or `null`, and one that differs from `harness` was ignored. `filter`: equality on top-level fields of the event's `data`. `once`: end after the first delivery. `replay`: deliver the latest event published within this long before subscribing (at most `"24h"`). `ttl`: end after this long without a delivery or a fresh subscribe (default `"30d"`). Durations are seconds or `"90s"`, `"5m"`, `"2h"`, `"30d"` (whole days). Subscribing again refreshes it. `result` returns your text (or `CallToolResult`) followed by Wakeshift's text, with its `_meta`. |
| `unsubscribe(call, topic?)` | How many of the calling conversation's subscriptions ended (all your topics without `topic`). |
| `unsubscribeUser(user, topic?)` | How many ended: every conversation of `user` stops getting `topic` (all your topics without it). Each conversation is told you ended it. For when someone loses access. |
| `endSubscription(subscriptionId)` | `true` if it ended one subscription (its conversation is told you ended it); `false` if it had already ended or isn't yours. |
| `publish(topics, { text, data?, id? })` | `{ matched, duplicate }`. `topics`: one or up to 20. `text` at most 1000 bytes; `data` a JSON object within the 64 KB request limit. The same `id` within a day publishes once (`duplicate: true`); after a day, it publishes again. |
| `send(subscriptionId, event)` | `"delivered"` (connected, on its way), `"queued"` (waits for the conversation) or `"closed"` (the subscription ended). Ignores the filter. |
| `topicStatus(topic)` | `{ topic, subscriptions, connected, via: { stream, poll, hooks, webhook } }`: how many conversations are subscribed, and how many are connected now and how (`stream`: a stream held open, like Cursor's background command; `poll`: long polls, waits or a wait tool, like Claude Code's Monitor; `hooks`: Wakeshift's hooks or connector on the user's machine; `webhook`: a ChatGPT chat monitoring the event through MCP Events). Only reads. |
| `subscriptionStatus(subscriptionId)` | `{ id, topic, status: "active" \| "closed", connected, via?, expiresAt? }`. Only reads. |
| `publishUrl(topic)` | `{ id, topic, url }`: anyone can POST to `url` to publish to `topic` (a JSON object body becomes `data`; `Idempotency-Key` or `X-GitHub-Delivery` dedupes). |
| `revokePublishUrl(id)` | The URL answers 410 from then on. |
| `annotate(call, result)` | A copy of `result` with what Wakeshift adds for this conversation (a fresh Codex connector beacon, messages waiting; nothing while what it was handed a moment ago is unused). Only results you pass here (and `sub.result`) change. `addition(call)` returns the raw `{ text?, meta?, agent }`. |
| `closeTopic(topic)` | How many subscriptions ended. Each conversation gets what was already published to it, then a notice. |
| `forgetUser(user)` | `false` if there was nothing to forget. Deletes everything Wakeshift keeps for the user: their conversations, subscriptions, events and deliveries, stream links (open streams close), recent publishes to their `u/<user>/…` topics and publish URLs for them, and their ID. Your app's log records it with `user_sha256`, the hex SHA-256 of `"<app>:<user>"`, never the ID. Their subscription IDs stay known as closed. |

### Errors

API errors throw `WakeshiftError` with `status` and `code`, the same codes as the HTTP API's (site/docs/http-api.md), plus `unreachable`:

| Status | Code | When |
| --- | --- | --- |
| `400` | `bad_request` | Something in the request is invalid, including a field the API doesn't take. The message says what. |
| `401` | `unauthenticated` | The assertion was refused: expired, reused, signed with another key, or for another `aud` (a key for another Wakeshift). The message says which. |
| `403` | `private_topic` | Subscribing a caller to another user's `u/<user>/…` topic. |
| `403` | `other_client` | The `session` key is a conversation in another MCP client of the same user. Call again without it. |
| `403` | `not_headless` | Topics need an app whose users are its own. |
| `404` | `unknown_subscription` | The subscription ID isn't one of your app's. |
| `404` | `unknown_publish_url` | The publish URL ID isn't one of your app's. |
| `413` | `too_large` | The request body is over 64 KB. The limit is on the whole body, so `data` must leave room for the rest of the request. |
| `0` | `unreachable` | No answer at all: a wrong `core`, a network problem, or the timeout. The message names the core it tried. |

`isCode(err, "private_topic")` checks one.

## Development

```sh
npm install
npm test          # unit tests (node:test)
npm run build     # dist/
make sdk-test     # from the repo root: unit tests, then end to end against a real core (needs Docker for Postgres)
```

The end-to-end test (`test/e2e.test.ts`, run by `scripts/e2e.sh`) starts a core with Go's `TestSDKHarness`, which registers a self-serve app and writes the core's URL and a test key to a file.
