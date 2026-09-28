package wordreview

import (
	"errors"
	"unicode/utf8"
)

const (
	MaxWordLen  = 255 // buddy_word_reviews.word is VARCHAR(255).
	maxFieldLen = 2000
)

// ValidateFields applies the same limits to manually saved, suggested, and
// correction-captured words. Callers trim the fields before validation.
func ValidateFields(word, meaning, example string) error {
	if word == "" {
		return errors.New("word is required")
	}
	if utf8.RuneCountInString(word) > MaxWordLen {
		return errors.New("word is too long")
	}
	if utf8.RuneCountInString(meaning) > maxFieldLen || utf8.RuneCountInString(example) > maxFieldLen {
		return errors.New("meaning/example is too long")
	}
	return nil
}
