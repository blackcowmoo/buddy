package newsfeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const sampleRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Sample Feed</title>
<item>
  <title>First &amp; Second</title>
  <link>https://example.com/a</link>
  <description><![CDATA[A short snippet. <a href="https://example.com/a">Continue reading</a>]]></description>
  <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
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
	wantPubDate := time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)
	if !got[0].PublishedAt.Equal(wantPubDate) {
		t.Errorf("PublishedAt = %v, want %v", got[0].PublishedAt, wantPubDate)
	}
	// The second item has no <pubDate> at all — must parse to the zero Time
	// rather than erroring or dropping the item.
	if !got[1].PublishedAt.IsZero() {
		t.Errorf("PublishedAt for an item with no pubDate = %v, want the zero Time", got[1].PublishedAt)
	}
}

func TestParseRSS_InvalidXML(t *testing.T) {
	if _, err := parseRSS("Test", []byte("not xml")); err == nil {
		t.Fatal("want error for invalid XML")
	}
}

func TestParsePubDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"RFC1123Z with zone offset", "Mon, 02 Jan 2006 15:04:05 -0700", time.Date(2006, time.January, 2, 15, 4, 5, 0, time.FixedZone("", -7*60*60))},
		{"RFC1123 with named zone", "Mon, 02 Jan 2006 15:04:05 GMT", time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)},
		{"empty", "", time.Time{}},
		{"garbage", "not a date", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePubDate(c.in)
			if !got.Equal(c.want) {
				t.Errorf("parsePubDate(%q) = %v, want %v", c.in, got, c.want)
			}
		})
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

// TestFetchCandidates_SortsNewestFirstAcrossFeeds guards the ordering
// httpserver.articleDrawHandler now relies on: it just takes the first
// not-yet-drawn candidate, trusting FetchCandidates to have already put the
// most recent story first regardless of which feed it came from or which
// feed answered first.
func TestFetchCandidates_SortsNewestFirstAcrossFeeds(t *testing.T) {
	const olderRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel><item>
  <title>Older story</title>
  <link>https://example.com/older</link>
  <description>d</description>
  <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
</item></channel></rss>`
	const newerRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel><item>
  <title>Newer story</title>
  <link>https://example.com/newer</link>
  <description>d</description>
  <pubDate>Wed, 04 Jan 2006 15:04:05 GMT</pubDate>
</item></channel></rss>`
	const undatedRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel><item>
  <title>Undated story</title>
  <link>https://example.com/undated</link>
  <description>d</description>
</item></channel></rss>`

	older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(olderRSS)) }))
	defer older.Close()
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(newerRSS)) }))
	defer newer.Close()
	undated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(undatedRSS)) }))
	defer undated.Close()

	orig := feeds
	defer func() { feeds = orig }()
	// Registered oldest-server-first so a pass-through (no sort) would fail
	// this test — the sort, not registration order, must decide the result.
	feeds = []struct {
		source string
		url    string
	}{
		{"Older", older.URL},
		{"Undated", undated.URL},
		{"Newer", newer.URL},
	}

	got, err := FetchCandidates(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d: %+v", len(got), got)
	}
	if got[0].URL != "https://example.com/newer" {
		t.Fatalf("got[0].URL = %q, want the newest-dated story first: %+v", got[0].URL, got)
	}
	if got[1].URL != "https://example.com/older" {
		t.Fatalf("got[1].URL = %q, want the older-dated story second: %+v", got[1].URL, got)
	}
	// Undated (zero PublishedAt) sorts as oldest of all, last in the list.
	if got[2].URL != "https://example.com/undated" {
		t.Fatalf("got[2].URL = %q, want the undated story last: %+v", got[2].URL, got)
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
