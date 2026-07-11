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

	// STT
	FastSTT          string // "mock" | "whisper"
	SlowSTT          string // "mock" | "whisper"
	WhisperBin       string
	WhisperFastModel string
	WhisperSlowModel string

	// LLM: any OpenAI-compatible chat-completions server (llama.cpp's
	// llama-server, vLLM, LM Studio, or the OpenAI API itself).
	LLMBaseURL      string // the /v1 root, e.g. http://localhost:8081/v1
	LLMAPIKey       string // optional bearer token
	LLMChatModel    string
	LLMCorrectModel string

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

	// Temporary audio backup (internal/audiostore): each incoming utterance's
	// raw PCM is streamed to an S3-compatible bucket, e.g. Ceph RGW. Disabled
	// (zero-setup default) unless S3Endpoint is set. PathStyle and
	// StorageClass exist specifically for Ceph: RGW commonly needs path-style
	// addressing (bucket in the URL path, not a virtual-host subdomain), and a
	// storage class lets the deployer steer these disposable recordings onto
	// a cheaper/temporary disk pool instead of the bucket's default.
	S3Endpoint     string
	S3PathStyle    bool
	S3AccessKey    string
	S3SecretKey    string
	S3Bucket       string
	S3StorageClass string
}

func Load() Config {
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

		LLMBaseURL:      env("BUDDY_LLM_BASE_URL", "http://localhost:8081/v1"),
		LLMAPIKey:       env("BUDDY_LLM_API_KEY", ""),
		LLMChatModel:    env("BUDDY_LLM_CHAT_MODEL", "local-model"),
		LLMCorrectModel: env("BUDDY_LLM_CORRECT_MODEL", "local-model"),

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
