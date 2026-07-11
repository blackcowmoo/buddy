# Buddy — streaming English free-talking tutor

A voice conversation web app for English practice. You speak, Buddy replies
(streamed + spoken via **kokoro-82M in the browser**), and a background track
re-transcribes with a higher-quality model and shows **grammar / vocabulary
corrections**.

- **Backend:** Go (single binary, no Python anywhere)
- **STT:** pluggable — `mock` (zero setup) → whisper.cpp (subprocess) → Vosk (streaming)
- **LLM:** any OpenAI-compatible server over local HTTP (llama.cpp's
  llama-server, vLLM, LM Studio, or the OpenAI API) — chat + correction
- **TTS:** kokoro-82M in the browser (WebGPU), so the server stays audio-free
- **Frontend:** React + Vite + TypeScript
- **One entry point:** the Go server serves the frontend and reverse-proxies to
  Vite in dev — no nginx.

**Toolchain:** Go **1.26.5**, Node **26.5.0** (`.nvmrc`), pnpm 9 (via corepack).
`GOTOOLCHAIN=auto` will fetch go1.26.5 automatically if your local Go is older.

## Two-track pipeline

```
                                    ┌─────────────────────────────────────────────┐
 browser                            │               Go backend                    │
┌──────────────────┐   WS(binary)   │  ┌──────────────┐                           │
│ mic → PCM 16k    │───utterance───▶│  │  FAST track  │  fast STT (mock/          │
│  (AudioWorklet)  │                │  │ (low latency)│  whisper-tiny/vosk)       │
│                  │                │  └──────┬───────┘          │                │
│                  │                │         ▼ transcript       ▼                │
│                  │◀──assistant────│   LLM.ChatStream(OpenAI API) ─ token stream │
│ kokoro-82M TTS   │   delta        │         │                                   │
│  (WebGPU) ◀──────│◀─assistant_done│         │                                   │
│                  │                │  ┌──────▼──────────────────────────┐        │
│ correction cards │◀─correction────│  │ REFINE track (goroutine)        │        │
│ refined subtitle │◀─refined_──────│  │ slow STT (whisper-large)        │        │
└──────────────────┘   transcript   │  │  → LLM grammar/context fix      │        │
                                    │  │  → upgrade session context      │        │
                                    │  └─────────────────────────────────┘        │
                                    └─────────────────────────────────────────────┘
```

- **FAST** gives the "real-time" feel: quick transcript + token-streamed reply.
- **REFINE** runs in the background: better transcription, correction feedback,
  and it rewrites the last user turn in the shared session so future replies use
  accurate context. This is the "middle LLM cleans the context" idea.

## Persistent per-user memory

Each learner's context survives reconnects and server restarts, not just the
current WebSocket connection:

- **Verbatim window** (`internal/session`): recent turns kept word-for-word,
  mistakes included — the LLM sees what the learner actually said. Grammar
  correction is shown as separate feedback, never silently rewritten into what
  the LLM sees.
- **Compaction**: once the window passes `BUDDY_MAX_HISTORY_MESSAGES`, the
  oldest half is folded by the LLM into a compact running summary (interests,
  goals, recurring mistakes, topics) — this is what keeps long-term storage and
  LLM context cheap as a conversation grows.
- **Storage** (`internal/store`): summary + verbatim window persist to
  **MySQL** (`MYSQL_*` env vars), keyed by user ID, saved on disconnect and
  every 30s. MySQL, not an embedded file DB, because this app is meant to run
  as multiple replicas in Kubernetes — a single-writer file on a PV can't do
  that (and a PV is typically RWO: only one pod could mount it anyway). The
  database is shared with other services, so the one table is created as
  `buddy_profiles` (a `buddy_` prefix) to avoid collisions; reads can be
  offloaded to a replica via `MYSQL_RO_HOSTNAME`. Bring your own MySQL; this
  repo doesn't run one for you.
- **Identity** (`internal/identity`, `BUDDY_IDENTITY_MODE`): `cookie` (default)
  is anonymous — a random ID in a long-lived cookie, zero setup for local dev,
  not real auth. `oidc` verifies the JWT carried in the `Authorization: Bearer`
  header directly against **Dex**'s OIDC discovery/JWKS endpoint
  (`BUDDY_OIDC_ISSUER_URL`, `BUDDY_OIDC_CLIENT_ID`). A proxy may still sit in
  front of the app (e.g. to translate a browser session cookie into that
  `Authorization` header — browsers send cookies automatically on the `/ws`
  handshake, unlike custom headers, so this is how the token gets there at
  all), but unlike a trust-the-header setup, the proxy isn't a trust boundary
  here: Dex's signature over the token is what's actually checked, so a
  request that reached the app directly with a forged header would still be
  rejected. The verified token's `email` claim becomes the user ID; if the
  header is missing or the token doesn't verify, the connection is refused
  (401) rather than falling back to a shared ID. Verifying a token is
  already a local, in-process check once the JWKS is cached (Dex is only
  re-contacted when an unrecognized key ID shows up), so this scales fine as-is;
  setting `REDIS_CLUSTER_HOST` additionally caches verification results in a
  Redis Cluster so the auth path stays fast/available even if Dex is slow or
  briefly down — see `internal/identity/cached_oidc.go`. Redis is purely an
  optimization layer: any Redis error just falls through to verifying
  directly, same as when it's unset.

## Quickstart (zero setup — pure `docker build`, mock STT, no models)

No host Go/Node toolchain needed. Build the self-contained image and run it:

```bash
docker build -t buddy .
docker run --rm -p 8080:8080 buddy
```

Open **http://localhost:8080**, type a sentence or click **🎙 Talk**. With no
models installed you'll get mock transcripts and an "(LLM offline)" echo — the
whole loop is exercised. Click **Enable voice** to download kokoro and hear
replies (Chrome recommended for WebGPU).

## Turn on the real engines

**LLM (chat + correction) — llama.cpp:**

```bash
# build llama.cpp, then run its OpenAI-compatible server:
llama-server -m ./models/your-model.gguf --port 8081
# server talks to http://localhost:8081/v1 by default (BUDDY_LLM_BASE_URL)
```

Any OpenAI-compatible endpoint works — point `BUDDY_LLM_BASE_URL` at vLLM, LM
Studio, or `https://api.openai.com/v1` (with `BUDDY_LLM_API_KEY`).

**Quality STT (whisper.cpp, no Python):**

```bash
git clone https://github.com/ggml-org/whisper.cpp && cd whisper.cpp && make
# put whisper-cli on PATH
sh ./models/download-ggml-model.sh tiny.en     # fast track
sh ./models/download-ggml-model.sh large-v3    # slow/quality track
# copy the .bin files into buddy/models/, then:
```

Point the container at your (externally managed) LLM and MySQL, and pass
config with `-e`:

```bash
docker run --rm -p 8080:8080 \
  --add-host host.docker.internal:host-gateway \
  -e BUDDY_LLM_BASE_URL=http://host.docker.internal:8081/v1 \
  -e MYSQL_RW_HOSTNAME=host.docker.internal -e MYSQL_PORT=3306 \
  -e MYSQL_USERNAME=buddy -e MYSQL_PASSWORD=buddy -e MYSQL_DATABASE=buddy \
  buddy
```

Check what's active any time: `curl localhost:8080/api/health`.

> External components (the LLM server, models, MySQL) are **not**
> orchestrated by this repo — you run and manage them yourself. This repo only
> builds and runs the app image via `docker build` / `docker run`.

## Deploying (environment variables)

This repo doesn't ship Kubernetes manifests — bring your own (Deployment,
Service, Dex, etc.). What it does provide is
every env var the binary reads (`internal/config/config.go` is authoritative;
this table is a deployment-focused summary).

**Required for a real (non-zero-setup) deployment:**

| Variable | Purpose |
|---|---|
| `BUDDY_ENV=prod` | Serves the embedded frontend instead of proxying to Vite. |
| `MYSQL_RW_HOSTNAME` | MySQL primary (read-write) host. Required — the server fails to start if it can't connect. The table it creates is `buddy_profiles` (prefixed so it can share a database with other services). |
| `MYSQL_USERNAME`, `MYSQL_PASSWORD`, `MYSQL_DATABASE` | Credentials and database for the store. `MYSQL_PORT` defaults to `3306`. |
| `MYSQL_RO_HOSTNAME` | Optional read replica; `Load` reads from it to offload the primary. Leave unset to read from the primary (strongly consistent). |
| `BUDDY_LLM_BASE_URL` | Your OpenAI-compatible endpoint (llama.cpp `llama-server`, vLLM, LM Studio, hosted API). Without it, chat/correction silently degrade to an offline echo. |
| `BUDDY_LLM_API_KEY` | Only if your LLM endpoint needs a bearer token (e.g. a hosted API). |
| `BUDDY_IDENTITY_MODE=oidc` | Switches from the anonymous local-dev cookie to verifying a Dex-issued JWT. |
| `BUDDY_OIDC_ISSUER_URL` | Dex's issuer URL, e.g. `https://dex.example.com`. The server fetches Dex's discovery document + JWKS from this at startup. |
| `BUDDY_OIDC_CLIENT_ID` | Expected token audience (default `buddy`). |

`oidc` mode verifies the JWT's signature, issuer, audience, and expiry against
Dex directly. A proxy in front of the app (translating a browser session
cookie into the `Authorization` header) is still a normal deployment shape,
but it's no longer a trust boundary the way `header` mode was — the app
verifies the token's signature itself, so a forged header sent straight to
the app would still be rejected.

**Optional, only relevant with `oidc` mode:**

| Variable | Purpose |
|---|---|
| `REDIS_CLUSTER_HOST` | Seed host of a **Redis Cluster** to cache verification results in (`internal/identity/cached_oidc.go`); go-redis discovers the rest of the cluster's nodes from it. Unset (default) means every check verifies the token directly, same as before this existed — fine, since that's already local once the JWKS is cached. Set it to keep the auth path fast/available if Dex ever gets slow or flaky; Redis is never a hard dependency, any Redis error just falls through to direct verification. No `BUDDY_` prefix — same convention as `MYSQL_*`, for a shared secret store to inject. |
| `REDIS_PORT` | Port for `REDIS_CLUSTER_HOST` (default `6379`); skip it if the host value already carries its own port. |
| `REDIS_PASSWORD` | Only if your cluster needs it. |

**Everything else is optional** (sane defaults, see `.env.example`):
`BUDDY_ADDR`, `BUDDY_FEEDBACK_LANG`, `BUDDY_MAX_HISTORY_MESSAGES`,
`BUDDY_LLM_CHAT_MODEL`/`BUDDY_LLM_CORRECT_MODEL`, `BUDDY_FAST_STT`/`BUDDY_SLOW_STT`
(and the matching `BUDDY_WHISPER_*` vars if you set either to `whisper`).
`BUDDY_WEB_DIST`/`BUDDY_VITE_URL` only matter in dev — a prod image embeds the
frontend and ignores them.

| `ROOT_PATH` | Mounts the whole app under a path prefix instead of `/`, e.g. `ROOT_PATH=/pr/14` for a PR-preview deployment that an external router sends `/pr/14/*` to. The app strips the prefix itself (`httpserver.withRootPath`); the frontend resolves its own asset/WS/worklet URLs relative to the page URL, so no rebuild is needed per prefix. Unset (default) mounts at `/`, unchanged. The hamburger menu's PR-path field (`lib/rootPath.ts`) lets a user jump straight to another `/pr/<n>/` deployment without typing the URL by hand. |

## Build outputs

The multi-stage `Dockerfile` builds the frontend with Node 26.5.0, embeds it into
the Go binary (`//go:embed`, built with `-tags embed`), and ships a static binary
on Alpine (a slim base that still includes `/bin/sh` for `docker exec`/`kubectl
exec` debugging), running as a non-root user. No Python, no runtime static dir,
no nginx.

```bash
docker build -t buddy .                                           # runnable image
docker build --target export --output type=local,dest=./bin .     # JUST the binary -> ./bin/buddy
```

The `export` stage is `FROM scratch`, so the extracted binary carries nothing
else. To run whisper STT inside the container, add the `whisper-cli` binary +
models to a runtime stage and set `BUDDY_FAST_STT=whisper`.

## Layout

```
buddy/
├── Dockerfile                   # multi-stage → self-contained binary
├── .dockerignore
├── .nvmrc                        # node 26.5.0
├── go.work                      # Go workspace (go 1.26.5)
├── pnpm-workspace.yaml          # JS workspace
├── apps/
│   ├── server/                  # Go backend (module: buddy/server)
│   │   ├── cmd/server/main.go   # composition root
│   │   └── internal/
│   │       ├── config/          # env config
│   │       ├── httpserver/      # single entry point: /ws, /api, static+proxy
│   │       ├── webassets/       # //go:embed frontend (embed / noembed tags)
│   │       ├── transport/       # WebSocket session loop (barge-in)
│   │       ├── protocol/        # wire types (mirrored in web/src/lib/protocol.ts)
│   │       ├── pipeline/        # FAST + REFINE orchestration
│   │       ├── session/         # per-connection memory: verbatim window + summary
│   │       ├── store/           # persists Profiles (MySQL, buddy_ table prefix)
│   │       ├── identity/        # resolves user ID: anonymous cookie, or OIDC (Dex JWT)
│   │       ├── stt/             # Recognizer interface: mock, whisper
│   │       ├── llm/             # Client interface: OpenAI-compatible (llama.cpp)
│   │       └── tts/             # (extension point; browser does TTS)
│   └── web/                     # React + Vite + TS
│       ├── public/pcm-worklet.js
│       └── src/
│           ├── audio/recorder.ts   # mic → 16k PCM
│           ├── tts/kokoro.ts       # kokoro-82M (WebGPU)
│           ├── lib/ws.ts           # WebSocket client
│           ├── lib/protocol.ts     # wire types
│           ├── lib/rootPath.ts     # PR-preview path switcher (hamburger menu)
│           ├── lib/me.ts           # fetches resolved identity for the menu
│           └── App.tsx             # + App.test.tsx (Testing Library, jsdom)
└── models/                      # ggml-*.bin etc. (gitignored)
```

## Extension seams (where to grow)

Everything heavy sits behind a small interface so you can swap implementations
without touching the pipeline:

| Want | Do this |
|------|---------|
| **True streaming partials** | Implement `stt.StreamingRecognizer` with the Vosk Go bindings; feed audio chunks instead of one-utterance frames and emit `partial_transcript`. |
| **Hands-free (auto VAD)** | Replace `PCMRecorder` with `@ricky0123/vad-web`; its `onSpeechEnd` hands you a 16 kHz `Float32Array` per utterance — pipe through `floatTo16()`. |
| **In-process STT (no subprocess)** | Swap `stt.Whisper` for the whisper.cpp CGo bindings; same `Recognizer` interface. |
| **Server-side TTS** | Implement `tts.Synthesizer` (e.g. shell out to piper) and stream audio frames down the socket for non-browser clients. |
| **Lower TTS latency** | Speak per sentence as `assistant_delta`s arrive instead of on `assistant_done`. |
| **Different LLM host** | `llm.OpenAI` already works with any OpenAI-compatible server (llama.cpp, vLLM, LM Studio, hosted APIs) — just change `BUDDY_LLM_BASE_URL`. |
| **Real auth** | Done: `BUDDY_IDENTITY_MODE=oidc` verifies a Dex-issued JWT from the `Authorization` header directly against Dex. For a different provider/setup, implement `identity.Identifier` — `internal/store` doesn't care where the ID came from. |

## Roadmap — what's still not done

- **True real-time voice.** Input is still push-to-talk: click to record, click
  to send the whole utterance as one WS frame. The protocol already has
  `partial_transcript` for this, but nothing emits it yet. Getting to an
  actually-continuous "just talk" experience needs both pieces from the
  Extension seams table above: hands-free VAD on the client (auto-detect
  speech start/end instead of a button) and a true streaming
  `stt.StreamingRecognizer` on the server (e.g. Vosk) that emits partials as
  audio arrives instead of waiting for one full utterance.
- **Frontend test coverage is partial.** The Go backend has a full suite
  (`go test ./...`, including real-MySQL integration tests) across every
  package with logic. `apps/web` now has `tsc` type-checking plus Vitest unit
  tests for pure logic (`prPath`, `fetchMe`, `floatTo16`) and Testing
  Library component tests for the hamburger menu (`App.test.tsx`, run under a
  per-file `jsdom` environment since the rest of the suite runs under
  `node`), but most of the conversation UI (message list, corrections,
  mic/TTS flows) still has no component tests.
- **Server-side TTS is just an interface, no implementation.**
  `tts.Synthesizer` exists as a seam but nothing implements it — fine today
  since the browser (kokoro-82M) handles all TTS, but needed for any
  non-browser client.
- **No `llm.Translate()` seam.** The correction prompt asks the LLM to emit
  Korean explanations directly in one call. That's simpler than a separate
  translation round-trip, but means "translate this other piece of UI text"
  isn't a reusable primitive yet if more localized surfaces get added later.
- **Kubernetes deployment is intentionally out of scope for this repo** — see
  "Deploying" above for the env vars; manifests and Dex config are owned by
  whoever deploys this, not tracked here.

## Notes

- Audio format on the wire: mono, 16 kHz, signed 16-bit little-endian PCM. The
  browser `AudioContext({ sampleRate: 16000 })` avoids resampling (Chrome).
- `protocol.go` and `protocol.ts` are hand-mirrored — change them together.
- Barge-in: speaking again cancels the in-flight turn (see `transport/ws.go`).
- kokoro's ONNX runtime is ~21 MB of WASM + the model on first load; it's lazy.
