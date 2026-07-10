package config

import "testing"

// allBuddyEnvVars lists every key Load() reads, so tests can force a clean
// slate regardless of what the host environment happens to have set.
var allBuddyEnvVars = []string{
	"BUDDY_ENV", "BUDDY_ADDR", "BUDDY_WEB_DIST", "BUDDY_VITE_URL",
	"BUDDY_FAST_STT", "BUDDY_SLOW_STT", "BUDDY_WHISPER_BIN",
	"BUDDY_WHISPER_FAST_MODEL", "BUDDY_WHISPER_SLOW_MODEL",
	"BUDDY_LLM_BASE_URL", "BUDDY_LLM_API_KEY", "BUDDY_LLM_CHAT_MODEL",
	"BUDDY_LLM_CORRECT_MODEL", "BUDDY_FEEDBACK_LANG",
	"BUDDY_DATABASE_URL", "BUDDY_MAX_HISTORY_MESSAGES",
	"BUDDY_IDENTITY_MODE", "BUDDY_AUTH_HEADER",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allBuddyEnvVars {
		t.Setenv(k, "") // env() treats "" as unset, same as a missing var
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	c := Load()

	str := map[string]struct{ got, want string }{
		"Env":             {c.Env, "dev"},
		"Addr":            {c.Addr, ":8080"},
		"FastSTT":         {c.FastSTT, "mock"},
		"SlowSTT":         {c.SlowSTT, "mock"},
		"LLMBaseURL":      {c.LLMBaseURL, "http://localhost:8081/v1"},
		"LLMChatModel":    {c.LLMChatModel, "local-model"},
		"LLMCorrectModel": {c.LLMCorrectModel, "local-model"},
		"FeedbackLang":    {c.FeedbackLang, "ko"},
		"DatabaseURL":     {c.DatabaseURL, "postgres://buddy:buddy@localhost:5432/buddy?sslmode=disable"},
		"IdentityMode":    {c.IdentityMode, "cookie"},
		"AuthHeader":      {c.AuthHeader, "X-Auth-Request-Email"},
	}
	for name, tc := range str {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
	}
	if c.MaxHistoryMessages != 20 {
		t.Errorf("MaxHistoryMessages = %d, want 20", c.MaxHistoryMessages)
	}
	if !c.IsDev() {
		t.Errorf("IsDev() = false, want true when BUDDY_ENV is unset (default dev)")
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("BUDDY_ENV", "prod")
	t.Setenv("BUDDY_ADDR", ":9999")
	t.Setenv("BUDDY_MAX_HISTORY_MESSAGES", "42")
	t.Setenv("BUDDY_FEEDBACK_LANG", "ja")
	t.Setenv("BUDDY_IDENTITY_MODE", "header")
	t.Setenv("BUDDY_AUTH_HEADER", "X-Forwarded-Email")

	c := Load()

	if c.Env != "prod" || c.IsDev() {
		t.Errorf("Env override failed: Env=%q IsDev()=%v", c.Env, c.IsDev())
	}
	if c.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", c.Addr)
	}
	if c.MaxHistoryMessages != 42 {
		t.Errorf("MaxHistoryMessages = %d, want 42", c.MaxHistoryMessages)
	}
	if c.FeedbackLang != "ja" {
		t.Errorf("FeedbackLang = %q, want ja", c.FeedbackLang)
	}
	if c.IdentityMode != "header" {
		t.Errorf("IdentityMode = %q, want header", c.IdentityMode)
	}
	if c.AuthHeader != "X-Forwarded-Email" {
		t.Errorf("AuthHeader = %q, want X-Forwarded-Email", c.AuthHeader)
	}
}

func TestEnvIntFallsBackOnInvalidValue(t *testing.T) {
	clearEnv(t)
	t.Setenv("BUDDY_MAX_HISTORY_MESSAGES", "not-a-number")
	c := Load()
	if c.MaxHistoryMessages != 20 {
		t.Errorf("MaxHistoryMessages = %d, want default 20 for an invalid value", c.MaxHistoryMessages)
	}
}

func TestIsDevCaseInsensitive(t *testing.T) {
	clearEnv(t)
	t.Setenv("BUDDY_ENV", "PROD")
	if Load().IsDev() {
		t.Error("IsDev() should be false for \"PROD\" (case-insensitive match)")
	}
}
