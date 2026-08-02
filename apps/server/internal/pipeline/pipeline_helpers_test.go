package pipeline

import (
	"testing"
)

func TestLanguageName(t *testing.T) {
	cases := map[string]string{
		"ko": "Korean", "KO-KR": "Korean", "en": "English", "ja": "Japanese",
		"zh": "Chinese", "es": "Spanish", "": "Korean", "auto": "Korean",
		"fr": "fr", // unknown code falls back to itself
	}
	for in, want := range cases {
		if got := languageName(in); got != want {
			t.Errorf("languageName(%q) = %q, want %q", in, got, want)
		}
	}
}
