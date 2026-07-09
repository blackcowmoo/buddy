# Buddy — streaming English free-talking tutor

A voice conversation web app for English practice. You speak, Buddy replies
(streamed + spoken via **kokoro-82M in the browser**), and a background track
re-transcribes with a higher-quality model and shows **grammar / vocabulary
corrections**.

- **Backend:** Go (single binary, no Python anywhere)
- **STT:** pluggable — `mock` (zero setup) → whisper.cpp (subprocess) → Vosk (streaming)
- **LLM:** Ollama over local HTTP (chat + correction)
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
│                  │◀─assistant_───│   LLM.ChatStream(Ollama) ── token stream   │
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

## Quickstart (zero setup — mock STT, no models)

```bash
# 1. deps
corepack enable pnpm      # one-time; ships with Node
make setup                # pnpm install + go mod tidy

# 2. run backend + frontend together
make dev                  # Go on :8080 proxies to Vite; open http://localhost:8080
```

Open **http://localhost:8080**, type a sentence or click **🎙 Talk**. With no
models installed you'll get mock transcripts and an "(LLM offline)" echo — the
whole loop is exercised. Click **Enable voice** to download kokoro and hear
replies (Chrome recommended for WebGPU).

## Turn on the real engines

**LLM (chat + correction):**

```bash
brew install ollama && ollama serve
ollama pull llama3.2:3b
# server auto-detects it at http://localhost:11434
```

**Quality STT (whisper.cpp, no Python):**

```bash
git clone https://github.com/ggml-org/whisper.cpp && cd whisper.cpp && make
# put whisper-cli on PATH
sh ./models/download-ggml-model.sh tiny.en     # fast track
sh ./models/download-ggml-model.sh large-v3    # slow/quality track
# copy the .bin files into buddy/models/, then:
```

```bash
cp .env.example .env
# set BUDDY_FAST_STT=whisper and BUDDY_SLOW_STT=whisper in .env
make dev
```

Check what's active any time: `curl localhost:8080/api/health`.

## Production build (Go serves the static bundle)

```bash
make run     # builds web → dist, builds the Go binary, serves everything on :8080
```

## Docker (single self-contained binary)

The multi-stage `Dockerfile` builds the frontend with Node 26.5.0, embeds it into
the Go binary (`//go:embed`, built with `-tags embed`), and ships a static binary
on distroless. No Python, no runtime static dir, no nginx.

```bash
make docker-image     # docker build -t buddy:latest .   (runnable image)
make docker-run       # build + run on :8080
make docker-binary    # extract JUST the binary  ->  ./bin/buddy
```

`make docker-binary` uses `docker build --target export --output` to copy the
compiled binary out to `./bin/buddy` — that's your "docker build makes the
binary" path. The `export` stage is `FROM scratch`, so nothing else is included.

The image runs mock STT by default; point it at a real LLM with
`docker run -e BUDDY_OLLAMA_URL=http://host.docker.internal:11434 -p 8080:8080 buddy:latest`.
To run whisper inside the container, add the `whisper-cli` binary + models to a
runtime stage and set `BUDDY_FAST_STT=whisper`.

## Layout

```
buddy/
├── Dockerfile                   # multi-stage → self-contained binary
├── .dockerignore
├── .nvmrc                        # node 26.5.0
├── go.work                      # Go workspace (go 1.26.5)
├── pnpm-workspace.yaml          # JS workspace
├── Makefile                     # dev / build / run / docker-*
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
│   │       ├── session/         # per-connection conversation memory
│   │       ├── stt/             # Recognizer interface: mock, whisper
│   │       ├── llm/             # Client interface: ollama
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
| **Different LLM host** | Implement `llm.Client` for llama.cpp-server / vLLM / a hosted API. |

## Notes

- Audio format on the wire: mono, 16 kHz, signed 16-bit little-endian PCM. The
  browser `AudioContext({ sampleRate: 16000 })` avoids resampling (Chrome).
- `protocol.go` and `protocol.ts` are hand-mirrored — change them together.
- Barge-in: speaking again cancels the in-flight turn (see `transport/ws.go`).
- kokoro's ONNX runtime is ~21 MB of WASM + the model on first load; it's lazy.
