package config

import (
	"slices"
	"testing"
)

// allBuddyEnvVars lists every key Load() reads, so tests can force a clean
// slate regardless of what the host environment happens to have set. The
// MySQL keys are unprefixed (they come from a shared secret store), but they're
// still read by Load() so they belong here.
var allBuddyEnvVars = []string{
	"BUDDY_ENV", "BUDDY_ADDR", "BUDDY_WEB_DIST", "BUDDY_VITE_URL",
	"BUDDY_FAST_STT", "BUDDY_SLOW_STT", "BUDDY_WHISPER_BIN",
	"BUDDY_WHISPER_FAST_MODEL", "BUDDY_WHISPER_SLOW_MODEL",
	"WHISPER_SERVER_URLS", "PARAKEET_SERVER_URLS",
	"BUDDY_LLM_API_KEY",
	"BUDDY_LLM_CHAT_URL", "BUDDY_LLM_ANALYSIS_URLS", "BUDDY_LLM_JUDGE_URL",
	"BUDDY_TTS_URL",
	"BUDDY_FEEDBACK_LANG",
	"MYSQL_RW_HOSTNAME", "MYSQL_RO_HOSTNAME", "MYSQL_PORT",
	"MYSQL_USERNAME", "MYSQL_PASSWORD", "MYSQL_DATABASE",
	"BUDDY_MAX_HISTORY_MESSAGES",
	"BUDDY_IDENTITY_MODE", "BUDDY_OIDC_ISSUER_URL", "BUDDY_OIDC_CLIENT_ID",
	"ROOT_PATH",
	"S3_ENDPOINT", "S3_PATH_STYLE", "S3_ACCESS_KEY", "S3_SECRET_KEY",
	"S3_BUCKET", "S3_STORAGE_CLASS",
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
		"Env":            {c.Env, "dev"},
		"Addr":           {c.Addr, ":8080"},
		"FastSTT":        {c.FastSTT, "mock"},
		"SlowSTT":        {c.SlowSTT, "mock"},
		"LLMChatURL":     {c.LLMChatURL, "http://localhost:8081/v1"},
		"LLMChatModel":   {c.LLMChatModel, "local-model"},
		"LLMJudgeURL":    {c.LLMJudgeURL, "http://localhost:8081/v1"},
		"LLMJudgeModel":  {c.LLMJudgeModel, "local-model"},
		"TTSURL":         {c.TTSURL, ""},
		"TTSVoice":       {c.TTSVoice, ""},
		"FeedbackLang":   {c.FeedbackLang, "ko"},
		"MySQLRWHost":    {c.MySQLRWHost, "localhost"},
		"MySQLROHost":    {c.MySQLROHost, ""},
		"MySQLUser":      {c.MySQLUser, "buddy"},
		"MySQLPassword":  {c.MySQLPassword, "buddy"},
		"MySQLDatabase":  {c.MySQLDatabase, "buddy"},
		"IdentityMode":   {c.IdentityMode, "cookie"},
		"OIDCIssuerURL":  {c.OIDCIssuerURL, ""},
		"OIDCClientID":   {c.OIDCClientID, "buddy"},
		"RootPath":       {c.RootPath, ""},
		"S3Endpoint":     {c.S3Endpoint, ""},
		"S3AccessKey":    {c.S3AccessKey, ""},
		"S3SecretKey":    {c.S3SecretKey, ""},
		"S3Bucket":       {c.S3Bucket, ""},
		"S3StorageClass": {c.S3StorageClass, ""},
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
	if c.S3PathStyle {
		t.Errorf("S3PathStyle = true, want false by default")
	}
	if c.STTEngines != nil {
		t.Errorf("STTEngines = %v, want nil (no server engine configured)", c.STTEngines)
	}
	if len(c.LLMAnalysisURLs) != 1 || c.LLMAnalysisURLs[0] != "http://localhost:8081/v1" {
		t.Errorf("LLMAnalysisURLs = %v, want a single default entry", c.LLMAnalysisURLs)
	}
	if len(c.LLMAnalysisModels) != 1 || c.LLMAnalysisModels[0] != "local-model" {
		t.Errorf("LLMAnalysisModels = %v, want a single default entry", c.LLMAnalysisModels)
	}
}

func TestLoadSTTEnginesCollectsEveryConfiguredEngine(t *testing.T) {
	clearEnv(t)
	t.Setenv("WHISPER_SERVER_URLS", "whisper-large-v3-turbo@http://w1:8082/v1, whisper-large-v3-turbo@http://w2:8082/v1")
	t.Setenv("PARAKEET_SERVER_URLS", "http://p1:8083/v1")

	c := Load()
	if len(c.STTEngines) != 2 {
		t.Fatalf("STTEngines = %+v, want 2 entries (whisper AND parakeet, both configured)", c.STTEngines)
	}
	whisper, parakeet := c.STTEngines[0], c.STTEngines[1]
	if whisper.Name != "whisper" {
		t.Fatalf("STTEngines[0].Name = %q, want whisper (checked before parakeet)", whisper.Name)
	}
	if want := []string{"http://w1:8082/v1", "http://w2:8082/v1"}; !slices.Equal(whisper.URLs, want) {
		t.Fatalf("whisper.URLs = %v, want %v", whisper.URLs, want)
	}
	if want := []string{"whisper-large-v3-turbo", "whisper-large-v3-turbo"}; !slices.Equal(whisper.Models, want) {
		t.Fatalf("whisper.Models = %v, want %v", whisper.Models, want)
	}
	if parakeet.Name != "parakeet" {
		t.Fatalf("STTEngines[1].Name = %q, want parakeet", parakeet.Name)
	}
	if want := []string{"http://p1:8083/v1"}; !slices.Equal(parakeet.URLs, want) {
		t.Fatalf("parakeet.URLs = %v, want %v", parakeet.URLs, want)
	}
	if want := []string{""}; !slices.Equal(parakeet.Models, want) {
		t.Fatalf("parakeet.Models = %v, want %v (bare url, no \"model@\" prefix)", parakeet.Models, want)
	}
}

func TestLoadSTTEnginesOnlyIncludesConfiguredOnes(t *testing.T) {
	clearEnv(t)
	t.Setenv("PARAKEET_SERVER_URLS", "http://p1:8083/v1")

	c := Load()
	if len(c.STTEngines) != 1 || c.STTEngines[0].Name != "parakeet" {
		t.Fatalf("STTEngines = %+v, want a single parakeet entry (whisper unset)", c.STTEngines)
	}
}

func TestLoadLLMAnalysisURLsParsesModelAtURLPairs(t *testing.T) {
	clearEnv(t)
	t.Setenv("BUDDY_LLM_ANALYSIS_URLS", "gemma-4-e4b@http://a:8081/v1, qwen3-6-35b-a3b@http://b:8081/v1 ,http://c:8081/v1")

	c := Load()
	wantURLs := []string{"http://a:8081/v1", "http://b:8081/v1", "http://c:8081/v1"}
	if !slices.Equal(c.LLMAnalysisURLs, wantURLs) {
		t.Fatalf("LLMAnalysisURLs = %v, want %v", c.LLMAnalysisURLs, wantURLs)
	}
	wantModels := []string{"gemma-4-e4b", "qwen3-6-35b-a3b", ""}
	if !slices.Equal(c.LLMAnalysisModels, wantModels) {
		t.Fatalf("LLMAnalysisModels = %v, want %v", c.LLMAnalysisModels, wantModels)
	}
}

func TestLoadLLMChatAndJudgeURLParseModelAtURLPair(t *testing.T) {
	clearEnv(t)
	t.Setenv("BUDDY_LLM_CHAT_URL", "gemma-4-e2b@http://localhost:8081/v1")
	t.Setenv("BUDDY_LLM_JUDGE_URL", "qwen3-6-35b-a3b@http://localhost:8085/v1")

	c := Load()
	if c.LLMChatModel != "gemma-4-e2b" || c.LLMChatURL != "http://localhost:8081/v1" {
		t.Fatalf("Chat model/url = %q/%q, want gemma-4-e2b/http://localhost:8081/v1", c.LLMChatModel, c.LLMChatURL)
	}
	if c.LLMJudgeModel != "qwen3-6-35b-a3b" || c.LLMJudgeURL != "http://localhost:8085/v1" {
		t.Fatalf("Judge model/url = %q/%q, want qwen3-6-35b-a3b/http://localhost:8085/v1", c.LLMJudgeModel, c.LLMJudgeURL)
	}
}

func TestLoadLLMChatURLBareURLOmitsModel(t *testing.T) {
	clearEnv(t)
	t.Setenv("BUDDY_LLM_CHAT_URL", "http://localhost:8081/v1")

	c := Load()
	if c.LLMChatModel != "" {
		t.Fatalf("LLMChatModel = %q, want empty for a bare url with no \"model@\" prefix", c.LLMChatModel)
	}
	if c.LLMChatURL != "http://localhost:8081/v1" {
		t.Fatalf("LLMChatURL = %q, want http://localhost:8081/v1", c.LLMChatURL)
	}
}

func TestLoadTTSURLParsesModelAtURLPairAndDefaultsToDisabled(t *testing.T) {
	clearEnv(t)

	c := Load()
	if c.TTSURL != "" || c.TTSVoice != "" {
		t.Fatalf("TTSURL/TTSVoice = %q/%q, want empty/empty (feature off) when BUDDY_TTS_URL is unset", c.TTSURL, c.TTSVoice)
	}

	t.Setenv("BUDDY_TTS_URL", "af_heart@http://localhost:8880/v1")
	c = Load()
	if c.TTSVoice != "af_heart" || c.TTSURL != "http://localhost:8880/v1" {
		t.Fatalf("TTSVoice/TTSURL = %q/%q, want af_heart/http://localhost:8880/v1", c.TTSVoice, c.TTSURL)
	}
}

func TestParseModelURLPair(t *testing.T) {
	cases := []struct {
		in, wantModel, wantURL string
	}{
		{"gemma-4-e2b@http://localhost:8081/v1", "gemma-4-e2b", "http://localhost:8081/v1"},
		{"http://localhost:8081/v1", "", "http://localhost:8081/v1"},
		{" gemma-4-e2b @ http://localhost:8081/v1 ", "gemma-4-e2b", "http://localhost:8081/v1"},
	}
	for _, tc := range cases {
		model, url := parseModelURLPair(tc.in)
		if model != tc.wantModel || url != tc.wantURL {
			t.Errorf("parseModelURLPair(%q) = (%q, %q), want (%q, %q)", tc.in, model, url, tc.wantModel, tc.wantURL)
		}
	}
}

func TestParseModelURLPairs(t *testing.T) {
	models, urls := parseModelURLPairs("gemma-4-e4b@http://a:8081/v1,http://b:8081/v1, qwen3@http://c:8081/v1 ")
	wantModels := []string{"gemma-4-e4b", "", "qwen3"}
	wantURLs := []string{"http://a:8081/v1", "http://b:8081/v1", "http://c:8081/v1"}
	if !slices.Equal(models, wantModels) {
		t.Fatalf("models = %v, want %v", models, wantModels)
	}
	if !slices.Equal(urls, wantURLs) {
		t.Fatalf("urls = %v, want %v", urls, wantURLs)
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
	t.Setenv("S3_ENDPOINT", "https://ceph.example.com")
	t.Setenv("S3_PATH_STYLE", "true")
	t.Setenv("S3_ACCESS_KEY", "access-123")
	t.Setenv("S3_SECRET_KEY", "secret-456")
	t.Setenv("S3_BUCKET", "buddy-recordings")
	t.Setenv("S3_STORAGE_CLASS", "REDUCED_REDUNDANCY")

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

	if c.S3Endpoint != "https://ceph.example.com" {
		t.Errorf("S3Endpoint = %q, want https://ceph.example.com", c.S3Endpoint)
	}
	if !c.S3PathStyle {
		t.Errorf("S3PathStyle = false, want true")
	}
	if c.S3AccessKey != "access-123" || c.S3SecretKey != "secret-456" {
		t.Errorf("S3 credentials = %q/%q, want access-123/secret-456", c.S3AccessKey, c.S3SecretKey)
	}
	if c.S3Bucket != "buddy-recordings" {
		t.Errorf("S3Bucket = %q, want buddy-recordings", c.S3Bucket)
	}
	if c.S3StorageClass != "REDUCED_REDUNDANCY" {
		t.Errorf("S3StorageClass = %q, want REDUCED_REDUNDANCY", c.S3StorageClass)
	}
}

func TestEnvBoolFallsBackOnInvalidValue(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_PATH_STYLE", "not-a-bool")
	c := Load()
	if c.S3PathStyle {
		t.Errorf("S3PathStyle = true, want default false for an invalid value")
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
