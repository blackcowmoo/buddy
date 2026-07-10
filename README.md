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
                                    ┌──────────────────────────────────────────┐
 browser                            │               Go backend                 │
┌──────────────────┐   WS(binary)  │  ┌──────────────┐                         │
│ mic → PCM 16k    │──utterance───▶│  │  FAST track  │  fast STT (mock/         │
│  (AudioWorklet)  │               │  │ (low latency)│  whisper-tiny/vosk)      │
│                  │               │  └──────┬───────┘         │               │
│                  │               │         ▼ transcript       ▼              │
│                  │◀─assistant_───│   LLM.ChatStream(OpenAI API) ─ token stream│
│ kokoro-82M TTS   │   delta        │         │                                 │
│  (WebGPU) ◀──────│◀─assistant_done│         │                                 │
│                  │               │  ┌──────▼────────────────────────┐         │
│ correction cards │◀─correction────│  │ REFINE track (goroutine)       │         │
│ refined subtitle │◀─refined_──────│  │ slow STT (whisper-large)       │         │
└──────────────────┘   transcript   │  │  → LLM grammar/context fix      │         │
                                    │  │  → upgrade session context      │         │
                                    │  └────────────────────────────────┘         │
                                    └──────────────────────────────────────────┘
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
  **PostgreSQL** (`BUDDY_DATABASE_URL`), keyed by user ID, saved on disconnect
  and every 30s. Postgres, not an embedded file DB, because this app is meant
  to run as multiple replicas in Kubernetes — a single-writer file on a PV
  can't do that (and a PV is typically RWO: only one pod could mount it
  anyway). Bring your own Postgres; this repo doesn't run one for you.
- **Identity** (`internal/identity`, `BUDDY_IDENTITY_MODE`): `cookie` (default)
  is anonymous — a random ID in a long-lived cookie, zero setup for local dev,
  not real auth. `header` trusts a header set by an upstream auth proxy that
  already authenticated the request — e.g. **oauth2-proxy in front of Dex** —
  configurable via `BUDDY_AUTH_HEADER` (default `X-Auth-Request-Email`). By
  design this is trust-the-header, not cryptographic verification: it's only
  as safe as the app being unreachable except through that proxy; enforce that
  with a NetworkPolicy / Service topology, not in this repo. If the configured
  header is missing, the connection is refused (401) rather than falling back
  to a shared ID.
  - **Testing `header` mode locally**: browsers can't set custom headers on a
    WebSocket upgrade from JS, so there's no way to exercise this path with a
    real browser without either a real oauth2-proxy or `BUDDY_DEV_AUTH_HEADER_VALUE`
    (dev-only — see `.env.example`), which makes the server inject the header
    itself. Structurally can't activate outside `BUDDY_ENV=dev`.

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

Point the container at your (externally managed) LLM and Postgres, and pass
config with `-e`:

```bash
docker run --rm -p 8080:8080 \
  --add-host host.docker.internal:host-gateway \
  -e BUDDY_LLM_BASE_URL=http://host.docker.internal:8081/v1 \
  -e BUDDY_DATABASE_URL=postgres://buddy:buddy@host.docker.internal:5432/buddy?sslmode=disable \
  buddy
```

Check what's active any time: `curl localhost:8080/api/health`.

> External components (the LLM server, models, Postgres) are **not**
> orchestrated by this repo — you run and manage them yourself. This repo only
> builds and runs the app image via `docker build` / `docker run`.

## Build outputs

The multi-stage `Dockerfile` builds the frontend with Node 26.5.0, embeds it into
the Go binary (`//go:embed`, built with `-tags embed`), and ships a static binary
on distroless. No Python, no runtime static dir, no nginx.

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
│   │       ├── store/           # persists Profiles (Postgres, pgx, no CGo)
│   │       ├── identity/        # resolves user ID (anonymous cookie today)
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
│           └── App.tsx
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
| **Real auth** | Done: `BUDDY_IDENTITY_MODE=header` trusts a header from an upstream auth proxy (oauth2-proxy + Dex). For a different setup, implement `identity.Identifier` — `internal/store` doesn't care where the ID came from. |

## Notes

- Audio format on the wire: mono, 16 kHz, signed 16-bit little-endian PCM. The
  browser `AudioContext({ sampleRate: 16000 })` avoids resampling (Chrome).
- `protocol.go` and `protocol.ts` are hand-mirrored — change them together.
- Barge-in: speaking again cancels the in-flight turn (see `transport/ws.go`).
- kokoro's ONNX runtime is ~21 MB of WASM + the model on first load; it's lazy.
