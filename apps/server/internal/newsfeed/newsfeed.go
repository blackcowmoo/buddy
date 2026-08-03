// Package newsfeed fetches candidate news articles from a curated list of
// well-known outlets' public RSS feeds — no API key or per-outlet scraper
// required, just the same lightweight XML syndication format every major
// outlet already publishes for feed readers. Deliberately shallow: an RSS
// item's own description is a short editorial snippet (a sentence or two),
// not the full article body — full-page scraping is fragile per outlet
// (paywalls, changing markup, anti-bot measures) and unnecessary here, since
// pipeline.GenerateArticleStudy only needs enough source material to write a
// coherent English summary paragraph from, not a verbatim copy of the page.
package newsfeed

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Candidate is one article pulled from a feed, before it's ever summarized
// or persisted (see newsarticle.Article, which a Candidate becomes once
// pipeline.GenerateArticleStudy runs on it).
type Candidate struct {
	Source      string // outlet name, e.g. "BBC"
	Title       string
	URL         string // canonical link; used as the de-dupe/cache key throughout
	Description string // the feed's own short snippet, not the full article body
}

// feeds is the curated list of outlets polled for candidates — every entry a
// stable, public RSS endpoint that needs no API key. A var (not const) so
// tests can swap it for a local httptest.Server instead of reaching the
// real internet. Adding a future outlet is a new row here, nothing else
// changes.
var feeds = []struct {
	source string
	url    string
}{
	{"BBC", "http://feeds.bbci.co.uk/news/world/rss.xml"},
	{"NPR", "https://feeds.npr.org/1004/rss.xml"},
	{"The Guardian", "https://www.theguardian.com/world/rss"},
}

// requestTimeout bounds one feed fetch — short, since this is a lightweight
// XML document a healthy server returns quickly, unlike llm.OpenAI's
// requestTimeout for a locally hosted model's completion.
const requestTimeout = 10 * time.Second

var httpClient = &http.Client{Timeout: requestTimeout}

type rssXML struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

// FetchCandidates polls every configured feed concurrently and returns every
// item found, deduped by URL (the same story is often syndicated with an
// identical link across an outlet's own sections). Best-effort per feed: one
// feed timing out, 404ing, or failing to parse doesn't fail the whole call —
// the pool only needs to be non-empty, not complete, so a caller (see
// httpserver's article-draw handler) can still draw from whichever feeds
// answered. Only an empty result across every feed is treated as an error.
func FetchCandidates(ctx context.Context) ([]Candidate, error) {
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []Candidate
	)
	seen := make(map[string]bool)
	for _, f := range feeds {
		wg.Add(1)
		go func(source, url string) {
			defer wg.Done()
			items, err := fetchFeed(ctx, source, url)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range items {
				if c.URL == "" || seen[c.URL] {
					continue
				}
				seen[c.URL] = true
				all = append(all, c)
			}
		}(f.source, f.url)
	}
	wg.Wait()
	if len(all) == 0 {
		return nil, fmt.Errorf("newsfeed: no candidates from %d configured feed(s)", len(feeds))
	}
	return all, nil
}

func fetchFeed(ctx context.Context, source, feedURL string) ([]Candidate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("newsfeed: %s: status %d", source, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseRSS(source, body)
}

// parseRSS decodes one feed's raw RSS/XML body into Candidates, split out
// from fetchFeed so parsing itself is unit-testable without an HTTP round
// trip.
func parseRSS(source string, body []byte) ([]Candidate, error) {
	var parsed rssXML
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("newsfeed: %s: parse: %w", source, err)
	}
	out := make([]Candidate, 0, len(parsed.Channel.Items))
	for _, it := range parsed.Channel.Items {
		out = append(out, Candidate{
			Source:      source,
			Title:       cleanText(it.Title),
			URL:         strings.TrimSpace(it.Link),
			Description: cleanText(it.Description),
		})
	}
	return out, nil
}

var htmlTagRegexp = regexp.MustCompile(`<[^>]*>`)

// cleanText strips embedded HTML markup (some feeds wrap description in a
// CDATA block containing a "<p>...</p>" or a trailing "<a href=...>Continue
// reading</a>") and decodes any remaining HTML entities (e.g. "&nbsp;",
// "&#8217;") that encoding/xml's own unescaping doesn't cover, since that
// only handles the five predefined XML entities.
func cleanText(s string) string {
	s = htmlTagRegexp.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}
