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

	// STT server engine: any OpenAI-compatible /v1/audio/transcriptions
	// server (whisper.cpp's `server` example, parakeet.cpp, or similar).
	// sttEngines (below) is checked in priority order; the first engine whose
	// *_URLS env var is set wins and its URLs/model land here. Comma-separated
	// URLs round-robin across replicas of the same engine. One model per
	// deployment, so the same engine serves both the fast and refine track.
	// Empty STTEngine (none configured) falls back to FastSTT/SlowSTT above.
	// Adding a future engine (e.g. parakeet.cpp) is an entry in sttEngines,
	// not a code change here.
	STTEngine string
	STTURLs   []string
	STTModel  string

	// LLM: any OpenAI-compatible chat-completions server (llama.cpp's
	// llama-server, vLLM, LM Studio, or the OpenAI API itself). Three
	// independent purposes, matching the two-track pipeline
	// (internal/pipeline.Pipeline):
	//   - Chat:     FAST track's streamed reply. One endpoint (*_URL).
	//   - Analysis: REFINE track's grammar-correction/compaction pass. Every
	//     configured endpoint is called concurrently as an ensemble, so this
	//     is comma-separated (*_URLS/*_MODELS, paired by index — a shorter
	//     model list repeats its last entry for the remaining URLs).
	//   - Judge:    synthesizes the analysis ensemble's outputs into the one
	//     result the pipeline uses. One endpoint (*_URL). Skipped when
	//     Analysis has a single candidate (pipeline.Pipeline.analyze) — a
	//     lone model has nothing to synthesize against.
	LLMAPIKey string // optional bearer token, shared by all of the above

	LLMChatURL   string
	LLMChatModel string

	LLMAnalysisURLs   []string
	LLMAnalysisModels []string

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

// sttEngines lists known STT server engines in priority order — Load() picks
// the first one whose *_URLS env var is set. No BUDDY_ prefix: these name
// the external engine/server itself (like WHISPER_SERVER_URLS), not a
// buddy-specific knob. Supporting a future engine (e.g. parakeet.cpp) is a
// new row here, nothing else changes.
var sttEngines = []struct {
	name     string
	urlsEnv  string
	modelEnv string
}{
	{"whisper", "WHISPER_SERVER_URLS", "WHISPER_SERVER_MODEL"},
	{"parakeet", "PARAKEET_SERVER_URLS", "PARAKEET_SERVER_MODEL"},
}

// loadSTTEngine picks the first configured server engine from sttEngines, or
// ("", nil, "") if none are set — Config.STTEngine stays empty and the
// server falls back to FastSTT/SlowSTT (mock/subprocess whisper).
func loadSTTEngine() (name string, urls []string, model string) {
	for _, e := range sttEngines {
		if v, ok := os.LookupEnv(e.urlsEnv); ok && v != "" {
			return e.name, splitCSV(v), env(e.modelEnv, "")
		}
	}
	return "", nil, ""
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

func Load() Config {
	sttName, sttURLs, sttModel := loadSTTEngine()

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

		STTEngine: sttName,
		STTURLs:   sttURLs,
		STTModel:  sttModel,

		LLMAPIKey: env("BUDDY_LLM_API_KEY", ""),

		LLMChatURL:   env("BUDDY_LLM_CHAT_URL", "http://localhost:8081/v1"),
		LLMChatModel: env("BUDDY_LLM_CHAT_MODEL", "local-model"),

		LLMAnalysisURLs:   splitCSV(env("BUDDY_LLM_ANALYSIS_URLS", "http://localhost:8081/v1")),
		LLMAnalysisModels: splitCSV(env("BUDDY_LLM_ANALYSIS_MODELS", "local-model")),

		LLMJudgeURL:   env("BUDDY_LLM_JUDGE_URL", "http://localhost:8081/v1"),
		LLMJudgeModel: env("BUDDY_LLM_JUDGE_MODEL", "local-model"),

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
