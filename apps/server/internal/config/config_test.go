package config

import "testing"

// allBuddyEnvVars lists every key Load() reads, so tests can force a clean
// slate regardless of what the host environment happens to have set. The
// MySQL keys are unprefixed (they come from a shared secret store), but they're
// still read by Load() so they belong here.
var allBuddyEnvVars = []string{
	"BUDDY_ENV", "BUDDY_ADDR", "BUDDY_WEB_DIST", "BUDDY_VITE_URL",
	"BUDDY_FAST_STT", "BUDDY_SLOW_STT", "BUDDY_WHISPER_BIN",
	"BUDDY_WHISPER_FAST_MODEL", "BUDDY_WHISPER_SLOW_MODEL",
	"BUDDY_LLM_BASE_URL", "BUDDY_LLM_API_KEY", "BUDDY_LLM_CHAT_MODEL",
	"BUDDY_LLM_CORRECT_MODEL", "BUDDY_FEEDBACK_LANG",
	"MYSQL_RW_HOSTNAME", "MYSQL_RO_HOSTNAME", "MYSQL_PORT",
	"MYSQL_USERNAME", "MYSQL_PASSWORD", "MYSQL_DATABASE",
	"BUDDY_MAX_HISTORY_MESSAGES",
	"BUDDY_IDENTITY_MODE", "BUDDY_OIDC_ISSUER_URL", "BUDDY_OIDC_CLIENT_ID",
	"ROOT_PATH",
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
		"MySQLRWHost":     {c.MySQLRWHost, "localhost"},
		"MySQLROHost":     {c.MySQLROHost, ""},
		"MySQLUser":       {c.MySQLUser, "buddy"},
		"MySQLPassword":   {c.MySQLPassword, "buddy"},
		"MySQLDatabase":   {c.MySQLDatabase, "buddy"},
		"IdentityMode":    {c.IdentityMode, "cookie"},
		"OIDCIssuerURL":   {c.OIDCIssuerURL, ""},
		"OIDCClientID":    {c.OIDCClientID, "buddy"},
		"RootPath":        {c.RootPath, ""},
	}
	for name, tc := range str {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
	}
	if c.MySQLPort != 3306 {
		t.Errorf("MySQLPort = %d, want 3306", c.MySQLPort)
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
	t.Setenv("BUDDY_IDENTITY_MODE", "oidc")
	t.Setenv("BUDDY_OIDC_ISSUER_URL", "https://dex.example.com")
	t.Setenv("BUDDY_OIDC_CLIENT_ID", "buddy-web")
	t.Setenv("MYSQL_RW_HOSTNAME", "primary.db")
	t.Setenv("MYSQL_RO_HOSTNAME", "replica.db")
	t.Setenv("MYSQL_PORT", "3307")
	t.Setenv("ROOT_PATH", "/pr/14")

	c := Load()

	if c.RootPath != "/pr/14" {
		t.Errorf("RootPath = %q, want /pr/14", c.RootPath)
	}

	if c.MySQLRWHost != "primary.db" || c.MySQLROHost != "replica.db" {
		t.Errorf("MySQL host overrides failed: rw=%q ro=%q", c.MySQLRWHost, c.MySQLROHost)
	}
	if c.MySQLPort != 3307 {
		t.Errorf("MySQLPort = %d, want 3307", c.MySQLPort)
	}

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
	if c.IdentityMode != "oidc" {
		t.Errorf("IdentityMode = %q, want oidc", c.IdentityMode)
	}
	if c.OIDCIssuerURL != "https://dex.example.com" {
		t.Errorf("OIDCIssuerURL = %q, want https://dex.example.com", c.OIDCIssuerURL)
	}
	if c.OIDCClientID != "buddy-web" {
		t.Errorf("OIDCClientID = %q, want buddy-web", c.OIDCClientID)
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

func TestRootPathNormalization(t *testing.T) {
	clearEnv(t)
	cases := map[string]string{
		"":          "",
		"/pr/14":    "/pr/14",
		"/pr/14/":   "/pr/14",
		"pr/14":     "/pr/14",
		"pr/14/":    "/pr/14",
		"  /pr/14 ": "/pr/14",
		"/":         "",
	}
	for in, want := range cases {
		t.Setenv("ROOT_PATH", in)
		if got := Load().RootPath; got != want {
			t.Errorf("normalizeRootPath(%q) = %q, want %q", in, got, want)
		}
	}
}
