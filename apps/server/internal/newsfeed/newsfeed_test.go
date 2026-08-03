package newsfeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const sampleRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Sample Feed</title>
<item>
  <title>First &amp; Second</title>
  <link>https://example.com/a</link>
  <description><![CDATA[A short snippet. <a href="https://example.com/a">Continue reading</a>]]></description>
</item>
<item>
  <title>Second Story</title>
  <link>https://example.com/b</link>
  <description>Another snippet with an entity: it&#8217;s here.</description>
</item>
</channel></rss>`

func TestParseRSS(t *testing.T) {
	got, err := parseRSS("Test", []byte(sampleRSS))
	if err != nil {
		t.Fatalf("parseRSS: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(got))
	}
	if got[0].Title != "First & Second" {
		t.Errorf("title not entity-decoded: %q", got[0].Title)
	}
	if got[0].URL != "https://example.com/a" {
		t.Errorf("unexpected url: %q", got[0].URL)
	}
	if got[0].Description != "A short snippet. Continue reading" {
		t.Errorf("description not stripped of markup: %q", got[0].Description)
	}
	if got[1].Description != "Another snippet with an entity: it’s here." {
		t.Errorf("description entity not decoded: %q", got[1].Description)
	}
	for _, c := range got {
		if c.Source != "Test" {
			t.Errorf("source not set: %+v", c)
		}
	}
}

func TestParseRSS_InvalidXML(t *testing.T) {
	if _, err := parseRSS("Test", []byte("not xml")); err == nil {
		t.Fatal("want error for invalid XML")
	}
}

func TestFetchCandidates_DedupesAndSkipsFailingFeeds(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sampleRSS))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	orig := feeds
	defer func() { feeds = orig }()
	// Two feeds return the exact same items (as if two outlets syndicated
	// the same story) plus one feed that always fails — the result must
	// still be non-empty and every URL must appear exactly once.
	feeds = []struct {
		source string
		url    string
	}{
		{"A", good.URL},
		{"B", good.URL},
		{"C", bad.URL},
	}

	got, err := FetchCandidates(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 deduped candidates, got %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.URL] {
			t.Errorf("duplicate URL in result: %s", c.URL)
		}
		seen[c.URL] = true
	}
}

func TestFetchCandidates_AllFeedsFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bad.Close()

	orig := feeds
	defer func() { feeds = orig }()
	feeds = []struct {
		source string
		url    string
	}{{"A", bad.URL}}

	if _, err := FetchCandidates(context.Background()); err == nil {
		t.Fatal("want error when every feed fails")
	}
}
