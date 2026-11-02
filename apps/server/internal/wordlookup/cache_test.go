package wordlookup

import (
	"strings"
	"testing"
)

func TestKeySeparatesArticleContextAndPosition(t *testing.T) {
	base := Request{ArticleID: "article-1", Word: "run", Position: 2, Context: "They run the company.", Language: "ko", Model: "model-a"}
	if Key(base) == Key(Request{ArticleID: "article-2", Word: base.Word, Position: base.Position, Context: base.Context, Language: base.Language, Model: base.Model}) {
		t.Fatal("lookup key reused across articles")
	}
	if Key(base) == Key(Request{ArticleID: base.ArticleID, Word: base.Word, Position: 3, Context: base.Context, Language: base.Language, Model: base.Model}) {
		t.Fatal("lookup key reused across word positions")
	}
	if Key(base) == Key(Request{ArticleID: base.ArticleID, Word: base.Word, Position: base.Position, Context: "They run every morning.", Language: base.Language, Model: base.Model}) {
		t.Fatal("lookup key reused across contexts")
	}
}

func TestKeyIsStableForIdenticalRequest(t *testing.T) {
	r := Request{ArticleID: "article-1", Word: "run", Position: 2, Context: "They run the company.", Language: "ko", Model: "model-a"}
	if Key(r) != Key(r) {
		t.Fatal("identical lookup request produced different keys")
	}
}

// The versioned namespace prevents results generated under an older prompt
// contract from hiding a newly generated memorization-ready definition.
func TestKeyUsesCurrentDefinitionContractVersion(t *testing.T) {
	r := Request{ArticleID: "article-1", Word: "facility", Position: 2, Context: "The facility produces steel.", Language: "ko", Model: "model-a"}
	if got := Key(r); !strings.HasPrefix(got, "buddy:word-lookup:v2:") {
		t.Fatalf("Key() = %q, want the current definition-contract namespace", got)
	}
}

// A hint is shown before (and independently of) any search, so its cache
// entry must never collide with the definition's: sharing one key would let
// the non-revealing hint answer the search, or let the search overwrite the
// hint namespace.
func TestHintKeySeparatesHintsFromDefinitions(t *testing.T) {
	r := Request{ArticleID: "article-1", Word: "break the ice", Position: 3, Context: "The host told a joke to break the ice.", Language: "ko", Model: "model-a"}
	if HintKey(r) == Key(r) {
		t.Fatal("hint and definition share one cache key")
	}
	if HintKey(r) != HintKey(r) {
		t.Fatal("identical hint request produced different keys")
	}
	if HintKey(r) == HintKey(Request{ArticleID: "article-2", Word: r.Word, Position: r.Position, Context: r.Context, Language: r.Language, Model: r.Model}) {
		t.Fatal("hint key reused across articles")
	}
	if got := HintKey(r); !strings.HasPrefix(got, "buddy:phrase-hint:v1:") {
		t.Fatalf("HintKey() = %q, want the hint-contract namespace", got)
	}
}
