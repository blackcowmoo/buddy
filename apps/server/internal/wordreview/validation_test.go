package wordreview

import (
	"strings"
	"testing"
)

func TestValidateFields(t *testing.T) {
	tests := []struct {
		name, word, meaning, example, wantError string
	}{
		{name: "word only", word: "resilient"},
		{name: "missing word", meaning: "뜻", wantError: "word is required"},
		{name: "ASCII limits", word: strings.Repeat("a", 255), meaning: strings.Repeat("b", 2000), example: strings.Repeat("c", 2000)},
		{name: "Unicode limits", word: strings.Repeat("가", 255), meaning: strings.Repeat("나", 2000), example: strings.Repeat("다", 2000)},
		{name: "long ASCII word", word: strings.Repeat("a", 256), wantError: "word is too long"},
		{name: "long Unicode word", word: strings.Repeat("가", 256), wantError: "word is too long"},
		{name: "long meaning", word: "word", meaning: strings.Repeat("뜻", 2001), wantError: "meaning/example is too long"},
		{name: "long example", word: "word", example: strings.Repeat("例", 2001), wantError: "meaning/example is too long"},
		{name: "word error first", word: strings.Repeat("a", 256), meaning: strings.Repeat("뜻", 2001), wantError: "word is too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFields(tt.word, tt.meaning, tt.example)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateFields() = %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantError {
				t.Fatalf("ValidateFields() = %v, want %q", err, tt.wantError)
			}
		})
	}
}
