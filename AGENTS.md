# Buddy contributor map

Buddy is a React/Vite client served by one Go HTTP/WebSocket binary. Start
with this file for code navigation; use `README.md` for deployment and
environment-variable details.

## Read by responsibility

| Concern | Primary location |
| --- | --- |
| Process wiring and worker startup | `apps/server/cmd/server` |
| Environment parsing | `apps/server/internal/config` |
| HTTP routes and request/response mapping | `apps/server/internal/httpserver` |
| WebSocket lifecycle and durable job adapters | `apps/server/internal/transport` |
| Conversation/STT/LLM behavior | `apps/server/internal/pipeline` |
| Redis job leases, retries, and inline fast paths | `apps/server/internal/asyncjob` |
| Translation/correction backfill | `apps/server/internal/backfill` |
| Chat/session persistence contracts and MySQL | `apps/server/internal/store` |
| Feature-owned persistence | `newsarticle`, `wordreview`, `writing`, `recording`, `ttsstore` |
| Browser app/session orchestration | `apps/web/src/App.tsx` |
| Browser view-only UI | `apps/web/src/components` and `apps/web/src/pages` |
| Browser API clients and pure state helpers | `apps/web/src/lib` |

Follow an HTTP feature from `httpserver/server.go` to its handler, then into a
domain store or a `transport` job. Follow live chat from `transport/ws.go` to
`pipeline`, whose emitted protocol events return through `transport/ws_persist.go`.
Protocol shapes shared across that boundary live in Go's `internal/protocol`
and TypeScript's `lib/protocol.ts`.

## Architecture invariants

- Persist every real conversation turn. On every reconnect, seed the in-memory
  turn counter from `Store.LastTurn`; reusing a turn number can overwrite
  history because turn rows are upserts.
- Long-running translation, correction, reply, study, vocabulary, and article
  work must survive navigation, WebSocket closure, and replica failure. With
  Redis, use `asyncjob`'s durable queue/lease path. Without Redis, use the
  existing detached inline fallback. Browser request handlers poll persisted
  status; they do not wait for LLM completion.
- Chat is the latency-sensitive LLM path. Analysis and Judge refine in the
  background; translation is lower priority and sequential. Preserve the
  Chat → Analysis → Judge publication rules documented in `README.md` and the
  relevant pipeline files.
- Distinct configured STT engines may run concurrently. Replicas of the same
  Whisper engine are alternatives selected by its HTTP transcriber, not a fan
  out that competes for the same CPU resource.
- Session deletion cascades to related S3 objects. Delete individual objects
  with the existing bounded concurrency helper; do not switch to bulk
  `DeleteObjects`, which is intentionally avoided for MinIO/Ceph compatibility.
- Ended rooms are immutable to the live chat path. Their summary and quiz may
  finish asynchronously and are surfaced through persisted status polling.

## Change guidelines

- Prefer the narrow capability interfaces in `internal/store` over the full
  composition-root `store.Store` when a consumer needs only one domain.
- Keep HTTP handlers focused on authentication, validation, and response
  mapping. Put durable execution in `transport`, model behavior in `pipeline`,
  and persistence rules in the owning store package.
- Keep `App.tsx` focused on cross-view/session coordination. Put display-only
  trees in `components`, page workflows in `pages`, and API/pure logic in `lib`.
- Preserve explicit async state (`pending`, `processing`, `done`, `failed`) and
  explain concurrency, persistence, or legacy compatibility with `why`
  comments. Do not add comments that merely restate code.
- Do not edit generated assets, dependency locks, or embedded build output
  unless the task specifically requires it.
- Every behavior change needs deterministic regression coverage. Never hide an
  unexplained failure with retries, sleeps, weaker assertions, or skips.
- Do not commit or push unless the current user request explicitly asks for it.

## Verification

Run the focused package/test while iterating, then the complete checks before
handoff:

```bash
(cd apps/server && go test ./...)
pnpm --filter @buddy/web typecheck
pnpm --filter @buddy/web test
pnpm --filter @buddy/web build
```

The repository expects the Node version declared in the root `package.json`.
Install workspace dependencies with `pnpm install --frozen-lockfile` when
`node_modules` is absent.
