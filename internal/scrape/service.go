package scrape

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Error is a scrape failure with a machine code, so the admin panel can say
// something specific -- "that site refused us" is a different instruction to
// the person reading it than "that link is dead".
type Error struct {
	Code    string
	Message string
	Detail  string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Scrape error codes.
const (
	CodeInvalidURL     = "INVALID_URL"
	CodeBlockedAddress = "BLOCKED_ADDRESS"
	CodeFetchFailed    = "FETCH_FAILED"
	CodePageBlocked    = "PAGE_BLOCKED"
	CodePageNotFound   = "PAGE_NOT_FOUND"
	CodeNoProduct      = "NO_PRODUCT_FOUND"
)

// Service reads product pages. Safe for concurrent use.
type Service struct {
	fetcher *Fetcher
	// proxyTemplate, when set, is a URL with a {url} placeholder used to
	// retry a page that refused us directly (a marketplace bot wall). It is
	// configuration rather than a hard-coded service because which reader
	// proxy is acceptable -- a paid scraping API, a self-hosted renderer,
	// none at all -- is a business decision, not this package's.
	proxyTemplate string
}

// NewService returns a Service. An empty proxyTemplate disables the retry.
func NewService(proxyTemplate string) *Service {
	return &Service{fetcher: NewFetcher(), proxyTemplate: strings.TrimSpace(proxyTemplate)}
}

// Scrape reads a product page and returns a draft for a human to approve.
//
// The order of extractors is deliberate, best source first: a site's own JSON
// beats the structured data it publishes for search engines, which beats the
// layout, which beats the social-sharing tags. Each one fills only the gaps
// its betters left, so a rich read is never degraded by a poor one.
func (s *Service) Scrape(ctx context.Context, rawURL string) (*Draft, error) {
	target, err := ParseTarget(rawURL)
	if err != nil {
		code := CodeInvalidURL
		if errors.Is(err, ErrBlockedAddress) {
			code = CodeBlockedAddress
		}
		return nil, &Error{Code: code, Message: err.Error()}
	}

	host := strings.ToLower(target.Hostname())

	// 1. The shop's own product JSON, when the URL shape suggests one exists.
	var draft *Draft
	if shopify := extractShopify(ctx, s.fetcher, target); shopify != nil {
		draft = shopify
	}

	page, fetchErr := s.fetchPage(ctx, target)
	if fetchErr != nil {
		// A Shopify read already succeeded without the HTML; the page fetch
		// failing afterwards is not worth discarding it for.
		if draft != nil {
			draft.SourceURL = target.String()
			draft.SourceSite = host
			draft.normalize()
			return draft, nil
		}
		return nil, fetchErr
	}

	extracted, err := extractAll(page.Body, page.URL, host)
	if err != nil {
		return nil, err
	}
	if extracted != nil {
		if draft == nil {
			draft = extracted
		} else {
			draft.fillFrom(extracted)
		}
	}

	if draft == nil || draft.Empty() {
		if looksBlocked(page.Body) {
			return nil, &Error{
				Code:    CodePageBlocked,
				Message: "That site would not show us the page -- it served a bot check instead.",
				Detail:  fmt.Sprintf("status %d", page.Status),
			}
		}
		return nil, &Error{
			Code:    CodeNoProduct,
			Message: "We could not find a product on that page. Check the link points at one item, not a search or category page.",
			Detail:  fmt.Sprintf("status %d", page.Status),
		}
	}

	draft.SourceURL = target.String()
	draft.SourceSite = host
	if page.Status != 200 {
		draft.Warnings = append(draft.Warnings,
			fmt.Sprintf("The page answered with status %d, so some details may be missing.", page.Status))
	}
	draft.normalize()
	draft.Attributes = guessAttributes(draft)
	return draft, nil
}

// ScrapeHTML reads a page an admin pasted in themselves.
//
// The escape hatch for sites that refuse a server: several brand stores sit
// behind a bot wall that answers 403 to anything without a browser's full
// fingerprint, and no amount of header-copying reliably gets past one. Rather
// than leave the admin retyping a spec table, they can view the page source in
// their own browser -- where they are a real, signed-in visitor -- and paste
// it here. Same extractors, same review step; only the fetch is different.
func (s *Service) ScrapeHTML(ctx context.Context, rawURL, pageHTML string) (*Draft, error) {
	if strings.TrimSpace(pageHTML) == "" {
		return nil, &Error{Code: CodeNoProduct, Message: "That paste was empty."}
	}

	target, err := ParseTarget(rawURL)
	if err != nil {
		// A URL is wanted but not required here: relative image paths cannot
		// be resolved without one, which is a gap in the draft rather than a
		// reason to refuse the paste.
		target = nil
	}

	host := ""
	if target != nil {
		host = strings.ToLower(target.Hostname())
	}

	draft, err := extractAll([]byte(pageHTML), target, host)
	if err != nil {
		return nil, err
	}
	if draft == nil || draft.Empty() {
		if looksBlocked([]byte(pageHTML)) {
			return nil, &Error{
				Code:    CodePageBlocked,
				Message: "That looks like the bot-check page rather than the product page. Open the product page in your browser first, then copy its source.",
			}
		}
		return nil, &Error{
			Code:    CodeNoProduct,
			Message: "We could not find a product in that page source.",
		}
	}

	if target != nil {
		draft.SourceURL = target.String()
	}
	draft.SourceSite = host
	draft.Extractor += " (pasted)"
	draft.normalize()
	draft.Attributes = guessAttributes(draft)
	return draft, nil
}

// extractAll runs every extractor over one document, best source first, each
// filling only the gaps its betters left.
func extractAll(body []byte, base *url.URL, host string) (*Draft, error) {
	doc, err := parseHTML(body)
	if err != nil {
		return nil, &Error{Code: CodeFetchFailed, Message: "That page could not be read.", Detail: err.Error()}
	}

	candidates := []*Draft{extractJSONLD(doc, base)}
	switch {
	case strings.Contains(host, "amazon."):
		// Amazon publishes no structured data, and its layout reader is the
		// only source of the gallery and spec table -- so there it leads.
		candidates = append([]*Draft{extractAmazon(doc, base)}, candidates...)
	case strings.Contains(host, "flipkart."):
		candidates = append(candidates, extractFlipkart(doc, base))
	case host == "":
		// A paste with no URL: try both marketplace readers, since neither
		// will find anything on a page it does not belong to.
		candidates = append(candidates, extractAmazon(doc, base), extractFlipkart(doc, base))
	}
	candidates = append(candidates, extractMeta(doc, base))

	var draft *Draft
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if draft == nil {
			draft = candidate
			continue
		}
		draft.fillFrom(candidate)
	}
	return draft, nil
}

// fetchPage gets the HTML, retrying through the configured reader proxy when
// the site refuses us directly.
func (s *Service) fetchPage(ctx context.Context, target *url.URL) (*Page, error) {
	page, err := s.fetcher.Get(ctx, target.String())
	if err == nil && page.Status < 400 && !looksBlocked(page.Body) {
		return page, nil
	}

	// A 404 is the admin's mistake to fix, not something a proxy will help
	// with, so it short-circuits.
	if err == nil && page.Status == 404 {
		return nil, &Error{
			Code:    CodePageNotFound,
			Message: "That page does not exist. Check the link.",
			Detail:  "status 404",
		}
	}

	if s.proxyTemplate != "" {
		if proxied, proxyErr := s.fetchViaProxy(ctx, target); proxyErr == nil {
			return proxied, nil
		}
	}

	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return nil, &Error{Code: CodeBlockedAddress, Message: "That address cannot be read from here.", Detail: err.Error()}
		}
		return nil, &Error{Code: CodeFetchFailed, Message: "We could not reach that page.", Detail: err.Error()}
	}
	return nil, &Error{
		Code:    CodePageBlocked,
		Message: "That site would not show us the page -- it served a bot check or an error instead.",
		Detail:  fmt.Sprintf("status %d", page.Status),
	}
}

// fetchViaProxy re-requests a page through the configured reader.
func (s *Service) fetchViaProxy(ctx context.Context, target *url.URL) (*Page, error) {
	proxied := strings.ReplaceAll(s.proxyTemplate, "{url}", url.QueryEscape(target.String()))
	// Readers that take the URL unescaped as a path suffix are common
	// (r.jina.ai/https://...), so support that spelling too.
	proxied = strings.ReplaceAll(proxied, "{rawUrl}", target.String())

	page, err := s.fetcher.Get(ctx, proxied)
	if err != nil {
		return nil, err
	}
	if page.Status >= 400 || looksBlocked(page.Body) {
		return nil, fmt.Errorf("proxy answered %d", page.Status)
	}
	// The page came from the proxy's host; links inside it still resolve
	// against the original, so rewrite the base before the extractors see it.
	page.URL = target
	return page, nil
}
