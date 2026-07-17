// Package config loads runtime configuration from environment variables.
// Every value has a sane default so the server boots with zero setup.
package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env  string // "dev" | "prod"
	Addr string // listen address, e.g. ":8080"

	// static / proxy
	WebDist string // dir served in prod
	ViteURL string // reverse-proxy target in dev

	// STT: legacy per-track switch, used only when no server engine below is
	// configured. "mock" (zero setup) or "whisper" (whisper.cpp subprocess,
	// internal/stt/whisper.go).
	FastSTT          string
	SlowSTT          string
	WhisperBin       string
	WhisperFastModel string
	WhisperSlowModel string

	// STT server engines: any OpenAI-compatible /v1/audio/transcriptions
	// server (whisper.cpp's `server` example, parakeet.cpp, or similar).
	// EVERY engine below whose *_URLS env var is set becomes one ensemble
	// member — all are called concurrently on each utterance
	// (pipeline.Pipeline.transcribe), not "first configured wins": STT
	// accuracy matters more than latency here, so disagreements between
	// engines get reconciled by an LLM instead of picking just one. Each
	// engine's own *_URLS entries are parsed by parseModelURLPairs ("model@url"
	// pairs, or a bare "url") and still round-robin internally across that
	// one engine's own replicas — the ensemble is across engines, replication
	// is within one. No STTEngines configured falls back to FastSTT/SlowSTT
	// above. Adding a future engine (e.g. parakeet.cpp) is a new row in
	// sttEngines, not a code change here.
	STTEngines []STTEngineConfig

	// LLM: any OpenAI-compatible chat-completions server (llama.cpp's
	// llama-server, vLLM, LM Studio, or the OpenAI API itself). Three
	// independent purposes, matching the two-track pipeline
	// (internal/pipeline.Pipeline), every one of them a "model@url" env var
	// (parseModelURLPair/parseModelURLPairs) rather than a *_URL + *_MODEL
	// pair kept in sync separately — same format whether the var holds one
	// endpoint or several, so there's exactly one place to look:
	//   - Chat:     FAST track's streamed reply. One endpoint (*_URL).
	//   - Analysis: REFINE track's grammar-correction/compaction pass. Every
	//     configured endpoint is called concurrently as an ensemble, so this
	//     one is comma-separated (*_URLS).
	//   - Judge:    synthesizes the analysis ensemble's outputs into the one
	//     result the pipeline uses. One endpoint (*_URL). Skipped when
	//     Analysis has a single candidate (pipeline.Pipeline.analyze) — a
	//     lone model has nothing to synthesize against.
	LLMAPIKey string // optional bearer token, shared by all of the above

	LLMChatURL   string
	LLMChatModel string

	LLMAnalysisURLs   []string
	LLMAnalysisModels []string // paired by index with LLMAnalysisURLs

	LLMJudgeURL   string
	LLMJudgeModel string

	// Feedback language: the learner's native language for correction
	// explanations (BCP-47-ish code, e.g. "ko", "en", "ja"). The corrected
	// sentence itself always stays in the target language (English).
	FeedbackLang string

	// Persistent per-user memory (internal/store, internal/session), backed by
	// MySQL so the app can run as multiple replicas in Kubernetes (unlike an
	// embedded single-writer file database). The database is shared with other
	// services, so buddy's one table is created with a buddy_ prefix to avoid
	// collisions. Reads can be offloaded to a replica by setting MySQLROHost;
	// leaving it empty serves reads from the primary (the strongly-consistent
	// default). These come from a shared secret store, so they use the plain
	// MYSQL_* names (no BUDDY_ prefix) that the platform already injects.
	MySQLRWHost   string // primary, read-write host (MYSQL_RW_HOSTNAME)
	MySQLROHost   string // optional read replica (MYSQL_RO_HOSTNAME); "" => use RW
	MySQLPort     int
	MySQLUser     string
	MySQLPassword string
	MySQLDatabase string

	MaxHistoryMessages int // verbatim turns kept before folding into the summary

	// Identity (internal/identity): "cookie" (default, anonymous, zero-setup
	// local dev) or "oidc" (verify the Dex-issued JWT carried in the
	// Authorization: Bearer header directly against Dex's discovery/JWKS
	// endpoint — no upstream auth proxy involved). OIDCIssuerURL/OIDCClientID
	// only apply to "oidc".
	IdentityMode  string
	OIDCIssuerURL string // Dex issuer, e.g. https://dex.example.com
	OIDCClientID  string // expected token audience

	// Optional Redis Cluster-backed cache of "oidc" verification results
	// (internal/identity/cached_oidc.go), so the WS-handshake/API auth path
	// stays fast even if Dex is slow or briefly unavailable. RedisClusterHost
	// empty (default) disables caching entirely — tokens are verified
	// directly on every call, same as before this existed. These come from
	// a shared secret store, so they use the plain REDIS_* names (no BUDDY_
	// prefix) the platform already injects — same convention as MYSQL_*.
	RedisClusterHost string // seed node; go-redis discovers the rest of the cluster from it
	RedisPort        int
	RedisPassword    string

	// RootPath mounts the whole app under a path prefix (e.g. "/pr/14"),
	// used for PR-preview deployments that route by URL path instead of a
	// header: an external router sends /pr/14/* to this instance unchanged,
	// and the app itself strips the prefix (see httpserver.withRootPath).
	// Empty (default) means the app is mounted at "/", unchanged.
	RootPath string

	// S3-compatible object storage, shared by two independent features that
	// both archive WS utterance audio to the same kind of endpoint:
	//   - internal/audiostore: each incoming utterance's raw PCM streamed to
	//     the bucket as a disposable, temporary backup (e.g. Ceph RGW) —
	//     nothing lists or plays it back.
	//   - internal/recording: the same audio, gzip-compressed WAV, archived
	//     with metadata in MySQL so it can be listed/replayed from the
	//     "recordings" page.
	// Both are disabled (zero-setup default) until S3Bucket is set. PathStyle
	// and StorageClass exist specifically for Ceph/MinIO-style endpoints: RGW
	// commonly needs path-style addressing (bucket in the URL path, not a
	// virtual-host subdomain), and a storage class lets the deployer steer
	// these onto a cheaper/temporary disk pool instead of the bucket's
	// default. These come from a shared secret store, so they use the plain
	// S3_* names (no BUDDY_ prefix), same convention as MYSQL_*/REDIS_*
	// above — static credentials, not the AWS default credential chain, so
	// this works the same whether the endpoint is real AWS S3 or a
	// self-hosted Ceph/MinIO cluster.
	S3Endpoint     string
	S3PathStyle    bool
	S3AccessKey    string
	S3SecretKey    string
	S3Bucket       string
	S3StorageClass string
}

// STTEngineConfig is one ensemble member for pipeline.Pipeline.STT: an
// engine's name (for logging/stt.HTTPTranscriber.Name()) and its
// "model@url" entries, already parsed by parseModelURLPairs.
type STTEngineConfig struct {
	Name   string
	URLs   []string
	Models []string // paired by index with URLs
}

// sttEngines lists known STT server engines — every one whose *_URLS env var
// is set becomes an STTEngineConfig (see loadSTTEngines). No BUDDY_ prefix:
// these name the external engine/server itself (like WHISPER_SERVER_URLS),
// not a buddy-specific knob. Supporting a future engine (e.g. parakeet.cpp)
// is a new row here, nothing else changes.
var sttEngines = []struct {
	name    string
	urlsEnv string
}{
	{"whisper", "WHISPER_SERVER_URLS"},
	{"parakeet", "PARAKEET_SERVER_URLS"},
}

// loadSTTEngines collects every configured server engine from sttEngines, or
// nil if none are set — Config.STTEngines stays empty and the server falls
// back to FastSTT/SlowSTT (mock/subprocess whisper).
func loadSTTEngines() []STTEngineConfig {
	var out []STTEngineConfig
	for _, e := range sttEngines {
		v, ok := os.LookupEnv(e.urlsEnv)
		if !ok || v == "" {
			continue
		}
		models, urls := parseModelURLPairs(v)
		out = append(out, STTEngineConfig{Name: e.name, URLs: urls, Models: models})
	}
	return out
}

// splitCSV parses a comma-separated env value into a trimmed, non-empty list.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseModelURLPairs parses a comma-separated list of "model@url" entries
// into parallel models/urls slices (same index i is one pair). A bare entry
// with no "@" is also accepted as just a url, with an empty model — callers
// that need a model name treat "" as "omit it" rather than guessing one.
//
// This is the format for every *_URLS env var (WHISPER_SERVER_URLS,
// BUDDY_LLM_ANALYSIS_URLS, ...): a comma list is presumed to allow a
// different model behind each endpoint, so pairing them via a second
// same-length *_MODELS list (kept in sync by index across two env vars) is
// both redundant and fragile. Folding the model into its own entry means
// there is exactly one thing to edit per endpoint. parseModelURLPair below
// is the singular counterpart, used by the *_URL vars (Chat, Judge) so a
// single endpoint follows the exact same format instead of falling back to
// a separate *_URL/*_MODEL pair — "model@url" either way, list or not.
func parseModelURLPairs(s string) (models, urls []string) {
	for _, part := range splitCSV(s) {
		if model, url, ok := strings.Cut(part, "@"); ok {
			models = append(models, strings.TrimSpace(model))
			urls = append(urls, strings.TrimSpace(url))
			continue
		}
		models = append(models, "")
		urls = append(urls, part)
	}
	return models, urls
}

// parseModelURLPair parses a single "model@url" value (or a bare "url" with
// no model) into (model, url) — see parseModelURLPairs for why every
// URL-shaped env var in this package shares this format.
func parseModelURLPair(s string) (model, url string) {
	if model, url, ok := strings.Cut(s, "@"); ok {
		return strings.TrimSpace(model), strings.TrimSpace(url)
	}
	return "", strings.TrimSpace(s)
}

func Load() Config {
	sttEngineConfigs := loadSTTEngines()
	chatModel, chatURL := parseModelURLPair(env("BUDDY_LLM_CHAT_URL", "local-model@http://localhost:8081/v1"))
	analysisModels, analysisURLs := parseModelURLPairs(env("BUDDY_LLM_ANALYSIS_URLS", "local-model@http://localhost:8081/v1"))
	judgeModel, judgeURL := parseModelURLPair(env("BUDDY_LLM_JUDGE_URL", "local-model@http://localhost:8081/v1"))

	return Config{
		Env:  env("BUDDY_ENV", "dev"),
		Addr: env("BUDDY_ADDR", ":8080"),

		WebDist: env("BUDDY_WEB_DIST", "apps/web/dist"),
		ViteURL: env("BUDDY_VITE_URL", "http://localhost:5173"),

		FastSTT:          env("BUDDY_FAST_STT", "mock"),
		SlowSTT:          env("BUDDY_SLOW_STT", "mock"),
		WhisperBin:       env("BUDDY_WHISPER_BIN", "whisper-cli"),
		WhisperFastModel: env("BUDDY_WHISPER_FAST_MODEL", "models/ggml-tiny.en.bin"),
		WhisperSlowModel: env("BUDDY_WHISPER_SLOW_MODEL", "models/ggml-large-v3.bin"),

		STTEngines: sttEngineConfigs,

		LLMAPIKey: env("BUDDY_LLM_API_KEY", ""),

		LLMChatURL:   chatURL,
		LLMChatModel: chatModel,

		LLMAnalysisURLs:   analysisURLs,
		LLMAnalysisModels: analysisModels,

		LLMJudgeURL:   judgeURL,
		LLMJudgeModel: judgeModel,

		FeedbackLang: env("BUDDY_FEEDBACK_LANG", "ko"),

		MySQLRWHost:   env("MYSQL_RW_HOSTNAME", "localhost"),
		MySQLROHost:   env("MYSQL_RO_HOSTNAME", ""),
		MySQLPort:     envInt("MYSQL_PORT", 3306),
		MySQLUser:     env("MYSQL_USERNAME", "buddy"),
		MySQLPassword: env("MYSQL_PASSWORD", "buddy"),
		MySQLDatabase: env("MYSQL_DATABASE", "buddy"),

		MaxHistoryMessages: envInt("BUDDY_MAX_HISTORY_MESSAGES", 20),

		IdentityMode:  env("BUDDY_IDENTITY_MODE", "cookie"),
		OIDCIssuerURL: env("BUDDY_OIDC_ISSUER_URL", ""),
		OIDCClientID:  env("BUDDY_OIDC_CLIENT_ID", "buddy"),

		RedisClusterHost: env("REDIS_CLUSTER_HOST", ""),
		RedisPort:        envInt("REDIS_PORT", 6379),
		RedisPassword:    env("REDIS_PASSWORD", ""),

		RootPath: normalizeRootPath(env("ROOT_PATH", "")),

		S3Endpoint:     env("S3_ENDPOINT", ""),
		S3PathStyle:    envBool("S3_PATH_STYLE", false),
		S3AccessKey:    env("S3_ACCESS_KEY", ""),
		S3SecretKey:    env("S3_SECRET_KEY", ""),
		S3Bucket:       env("S3_BUCKET", ""),
		S3StorageClass: env("S3_STORAGE_CLASS", ""),
	}
}

func (c Config) IsDev() bool { return strings.ToLower(c.Env) != "prod" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// normalizeRootPath cleans a path-prefix env value into the canonical shape
// httpserver.withRootPath expects: "" (unset) or "/foo" with no trailing
// slash, regardless of how the deployer wrote it (with/without a leading or
// trailing slash).
func normalizeRootPath(v string) string {
	v = strings.TrimSuffix(strings.TrimSpace(v), "/")
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	return v
}
