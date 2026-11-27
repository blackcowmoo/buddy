package nuance

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeRequestedWords(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  []string
		ok    bool
	}{
		{"plain pair", []string{"cheap", "inexpensive"}, []string{"cheap", "inexpensive"}, true},
		{"trims and lowercases", []string{" Cheap ", "INEXPENSIVE"}, []string{"cheap", "inexpensive"}, true},
		{"inner hyphen and apostrophe", []string{"self-employed", "don't"}, []string{"self-employed", "don't"}, true},
		{"one word", []string{"cheap"}, nil, false},
		{"three words", []string{"cheap", "inexpensive", "affordable"}, nil, false},
		{"same word twice", []string{"Cheap", "cheap"}, nil, false},
		{"blank entry", []string{"cheap", "   "}, nil, false},
		{"space-separated phrase", []string{"take", "take off"}, nil, false},
		{"non-latin text", []string{"싸다", "cheap"}, nil, false},
		{"digits", []string{"cheap", "w3ird"}, nil, false},
		{"leading hyphen", []string{"-cheap", "inexpensive"}, nil, false},
		{"overlong word", []string{strings.Repeat("a", maxRequestedWordLen+1), "cheap"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeRequestedWords(tt.words)
			if ok != tt.ok || !slices.Equal(got, tt.want) {
				t.Fatalf("NormalizeRequestedWords(%q) = (%q, %v), want (%q, %v)", tt.words, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestMatchesRequestedWordSet(t *testing.T) {
	c := testContent() // cheap + inexpensive
	if !c.MatchesRequestedWordSet([]string{"inexpensive", "cheap"}) {
		t.Fatal("order-insensitive match rejected")
	}
	if c.MatchesRequestedWordSet([]string{"cheap", "affordable"}) {
		t.Fatal("different set accepted")
	}
	if c.MatchesRequestedWordSet([]string{"cheap"}) {
		t.Fatal("shorter set accepted")
	}
	three := testContent()
	three.Words = append(three.Words, Word{Word: "affordable"})
	if three.MatchesRequestedWordSet([]string{"cheap", "inexpensive"}) {
		t.Fatal("extra lesson word accepted")
	}
}
