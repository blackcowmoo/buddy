package wordlookup

import "testing"

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
