package grok

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	grokRenderRE = regexp.MustCompile(`<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)
	spaceRE      = regexp.MustCompile(`\s+`)
)

// citationRef is a citation marker written into the text that still has to be
// located (to compute its annotation span).
type citationRef struct {
	URL    string
	Title  string
	Needle string
}

// citationSpan is a located citation: Start/End are byte offsets into the
// cleaned token.
type citationSpan struct {
	URL, Title string
	Start, End int
}

// citationState numbers sources in order of first citation and collapses
// consecutive citations of the same source.
type citationState struct {
	order   []string
	index   map[string]int
	last    int // last emitted citation number, 0 when none
	pending []citationRef
}

func newCitationState() citationState {
	return citationState{index: map[string]int{}}
}

// number returns the 1-based citation number of url, assigning the next one on
// first use.
func (c *citationState) number(url string) int {
	if n, ok := c.index[url]; ok {
		return n
	}
	c.order = append(c.order, url)
	n := len(c.order)
	c.index[url] = n
	return n
}

// renderToken rewrites the <grok:render> tags of a text token and returns the
// cleaned text plus the located citation spans.
func (s *StreamAdapter) renderToken(token string) (string, []citationSpan) {
	if !strings.Contains(token, "<grok:render") {
		return token, nil
	}
	cleaned := grokRenderRE.ReplaceAllStringFunc(token, func(match string) string {
		sub := grokRenderRE.FindStringSubmatch(match)
		if len(sub) < 4 {
			return ""
		}
		card := s.cards[sub[1]]
		if card == nil {
			return ""
		}
		switch sub[3] {
		case "render_searched_image":
			return renderSearchedImage(card)
		case "render_inline_citation":
			return s.renderInlineCitation(card)
		}
		// render_generated_image and unknown types render as nothing: the
		// image itself arrives as an image event.
		return ""
	})
	if strings.HasPrefix(cleaned, "\n") && strings.Contains(cleaned, "[[") {
		cleaned = strings.TrimLeft(cleaned, "\n")
	}
	return cleaned, s.locateCitations(cleaned)
}

// renderSearchedImage renders an image search result as markdown.
func renderSearchedImage(card map[string]any) string {
	img, _ := card["image"].(map[string]any)
	title, _ := img["title"].(string)
	if title == "" {
		title = "image"
	}
	thumb, _ := img["thumbnail"].(string)
	if thumb == "" {
		thumb, _ = img["original"].(string)
	}
	if link, _ := img["link"].(string); link != "" {
		return fmt.Sprintf("[![%s](%s)](%s)", title, thumb, link)
	}
	return fmt.Sprintf("![%s](%s)", title, thumb)
}

// renderInlineCitation renders a numbered citation link and queues its marker
// for span location. A citation of the source cited immediately before it
// renders as nothing.
func (s *StreamAdapter) renderInlineCitation(card map[string]any) string {
	u, _ := card["url"].(string)
	if u == "" {
		return ""
	}
	n := s.citations.number(u)
	if n == s.citations.last {
		return ""
	}
	s.citations.last = n
	needle := fmt.Sprintf(" [[%d]](%s)", n, u)

	title, _ := card["title"].(string)
	if title == "" {
		title = s.sources.titleFor(u)
	}
	if title == "" {
		title = u
	}
	s.citations.pending = append(s.citations.pending, citationRef{URL: u, Title: title, Needle: needle})
	return needle
}

// locateCitations finds the queued citation markers in cleaned, in order, and
// clears the queue.
func (s *StreamAdapter) locateCitations(cleaned string) []citationSpan {
	if len(s.citations.pending) == 0 {
		return nil
	}
	var spans []citationSpan
	searchStart := 0
	for _, cite := range s.citations.pending {
		pos := strings.Index(cleaned[searchStart:], cite.Needle)
		if pos < 0 {
			continue
		}
		pos += searchStart
		spans = append(spans, citationSpan{URL: cite.URL, Title: cite.Title, Start: pos, End: pos + len(cite.Needle)})
		searchStart = pos + len(cite.Needle)
	}
	s.citations.pending = s.citations.pending[:0]
	return spans
}

// sourceCollector gathers the search results (web and X posts) a response
// consulted, deduplicated by URL, in arrival order.
type sourceCollector struct {
	seen    map[string]struct{}
	results []map[string]any
}

func newSourceCollector() sourceCollector {
	return sourceCollector{seen: map[string]struct{}{}}
}

// addWeb records the items of a webSearchResults object.
func (c *sourceCollector) addWeb(raw any) {
	for _, item := range resultItems(raw) {
		u, _ := item["url"].(string)
		if u == "" || c.seenURL(u) {
			continue
		}
		item["type"] = "web"
		c.results = append(c.results, item)
	}
}

// addX records the items of an xSearchResults object as x.com post links.
func (c *sourceCollector) addX(raw any) {
	for _, item := range resultItems(raw) {
		postID, _ := item["postId"].(string)
		username, _ := item["username"].(string)
		if postID == "" || username == "" {
			continue
		}
		u := fmt.Sprintf("https://x.com/%s/status/%s", username, postID)
		if c.seenURL(u) {
			continue
		}
		text, _ := item["text"].(string)
		c.results = append(c.results, map[string]any{
			"url":   u,
			"title": xPostTitle(username, text),
			"type":  "x_post",
		})
	}
}

// seenURL marks u as seen and reports whether it already was.
func (c *sourceCollector) seenURL(u string) bool {
	if _, ok := c.seen[u]; ok {
		return true
	}
	c.seen[u] = struct{}{}
	return false
}

// resultItems extracts the object items of a {"results":[...]} value.
func resultItems(raw any) []map[string]any {
	obj, _ := raw.(map[string]any)
	list, _ := obj["results"].([]any)
	items := make([]map[string]any, 0, len(list))
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items
}

// xPostTitle builds the display title of an X post: the handle plus the post
// text collapsed to one line and cut at 50 characters (not bytes, so CJK text
// is never split inside a rune).
func xPostTitle(username, text string) string {
	text = strings.TrimSpace(spaceRE.ReplaceAllString(text, " "))
	if text == "" {
		return "𝕏/@" + username
	}
	if runes := []rune(text); len(runes) > 50 {
		return fmt.Sprintf("𝕏/@%s: %s...", username, string(runes[:50]))
	}
	return fmt.Sprintf("𝕏/@%s: %s", username, text)
}

// titleFor returns the title of the collected source with url ("" if none).
func (c *sourceCollector) titleFor(url string) string {
	for _, item := range c.results {
		if u, _ := item["url"].(string); u == url {
			if t, _ := item["title"].(string); t != "" {
				return t
			}
			return ""
		}
	}
	return ""
}

// list returns the structured sources (url, title, type) collected so far.
func (c *sourceCollector) list() []map[string]any {
	if len(c.results) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(c.results))
	for _, item := range c.results {
		u, _ := item["url"].(string)
		title, _ := item["title"].(string)
		if title == "" {
			title = u
		}
		typ, _ := item["type"].(string)
		if typ == "" {
			typ = "web"
		}
		out = append(out, map[string]any{"url": u, "title": title, "type": typ})
	}
	return out
}
