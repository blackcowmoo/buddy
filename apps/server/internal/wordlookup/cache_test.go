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
