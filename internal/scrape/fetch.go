// Package scrape turns a public product page into a draft product record.
//
// It exists so the catalogue can be filled from a URL -- an Amazon listing, a
// Flipkart page, a brand's own store -- instead of by retyping every
// specification by hand. Nothing here writes to the catalogue: the result is a
// draft for a human to correct and approve, and the honest answer "we could
// not read that page" is a normal outcome rather than an error to paper over.
package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Fetcher performs outbound requests on behalf of an admin-supplied URL.
//
// Every request here is server-side request forgery waiting to happen: the
// destination is chosen by a user, and this process sits inside a VPC with an
// instance metadata endpoint and a Mongo connection. So the dialer refuses any
// address outside the public internet, on the original request and on every
// redirect hop, and the body is capped -- a URL that streams forever must not
// take the process with it.
type Fetcher struct {
	client   *http.Client
	maxBytes int64
	// userAgent identifies us honestly as a browser-equivalent client. Sites
	// serve markup to browsers and a bot page (or nothing) to a default Go
	// user agent, so the honest-but-useless option is to be handed an empty
	// page every time.
	userAgent string
}

// DefaultUserAgent mirrors a current desktop Chrome on macOS.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

const (
	defaultTimeout  = 20 * time.Second
	defaultMaxBytes = 5 << 20 // 5 MiB of HTML is already an outlier.
	maxRedirects    = 5
)

// ErrBlockedAddress is returned when a URL resolves to an address this process
// must never reach: loopback, link-local (the cloud metadata endpoint lives
// there), private ranges, or anything that is not a public unicast IP.
var ErrBlockedAddress = errors.New("destination address is not permitted")

// NewFetcher returns a Fetcher with the standard timeouts and guards.
func NewFetcher() *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	transport := &http.Transport{
		// The check belongs here rather than on the URL's hostname: a
		// hostname is only a promise, and "localtest.me" or a DNS record
		// pointed at 169.254.169.254 resolves to somewhere we must not go.
		// Checking the address we are about to connect to is the only test
		// that cannot be spoofed by the name.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				if !isPublicUnicast(ip.IP) {
					lastErr = fmt.Errorf("%w: %s", ErrBlockedAddress, ip.IP)
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err != nil {
					lastErr = err
					continue
				}
				return conn, nil
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("%w: %s", ErrBlockedAddress, host)
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}

	return &Fetcher{
		client: &http.Client{
			Timeout:   defaultTimeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				// Redirects are the classic way around an allowlist: the first
				// hop is a harmless public host, the second is the metadata
				// service. The scheme is re-checked because http(s) is the
				// only thing we are willing to speak.
				return checkScheme(req.URL)
			},
		},
		maxBytes:  defaultMaxBytes,
		userAgent: DefaultUserAgent,
	}
}

// Page is a fetched document plus where it actually came from.
type Page struct {
	// URL after redirects. Relative links in the body resolve against this,
	// not against what was typed.
	URL         *url.URL
	Status      int
	ContentType string
	Body        []byte
}

// HTML reports whether the response looks like markup rather than JSON or an
// image; the extractors that parse tags must not be handed a binary.
func (p *Page) HTML() bool {
	return strings.Contains(strings.ToLower(p.ContentType), "html")
}

// Get fetches a URL as a browser would.
//
// A non-2xx response is returned rather than turned into an error: a 404 from
// a mistyped URL and a 503 from a bot wall need different words in front of an
// admin, and only the caller knows which it is looking at.
func (f *Fetcher) Get(ctx context.Context, raw string) (*Page, error) {
	target, err := ParseTarget(raw)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}

	// A plausible browser header set. Sites vary what they serve by these, and
	// an en-IN client is what both marketplaces expect for .in domains --
	// asking as anything else invites a currency and locale we would then have
	// to unpick.
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-IN,en-GB;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")

	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, f.maxBytes))
	if err != nil {
		return nil, err
	}

	final := res.Request.URL
	if final == nil {
		final = target
	}

	return &Page{
		URL:         final,
		Status:      res.StatusCode,
		ContentType: res.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

// GetJSON fetches a URL that is expected to answer with JSON, announcing that
// preference. Shopify storefronts key off Accept for their product endpoints.
func (f *Fetcher) GetJSON(ctx context.Context, raw string) (*Page, error) {
	target, err := ParseTarget(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "application/json,text/javascript,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-IN,en;q=0.9")

	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, f.maxBytes))
	if err != nil {
		return nil, err
	}
	final := res.Request.URL
	if final == nil {
		final = target
	}
	return &Page{URL: final, Status: res.StatusCode, ContentType: res.Header.Get("Content-Type"), Body: body}, nil
}

// GetBinary fetches an asset (an image) with a caller-chosen size cap.
//
// Separate from Get because the cap differs by an order of magnitude and
// because nothing here should ever try to parse the result as markup.
func (f *Fetcher) GetBinary(ctx context.Context, raw string, maxBytes int64, referer string) (*Page, error) {
	target, err := ParseTarget(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	if referer != "" {
		// Some CDNs serve a placeholder to requests with no referer. Sending
		// the page the image was found on is both accurate and what a browser
		// rendering that page would have sent.
		req.Header.Set("Referer", referer)
	}

	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// One byte past the cap, so a file that is exactly at the limit is
	// accepted and one over it is detectably truncated rather than silently
	// half-saved.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("image is larger than %d bytes", maxBytes)
	}

	final := res.Request.URL
	if final == nil {
		final = target
	}
	return &Page{URL: final, Status: res.StatusCode, ContentType: res.Header.Get("Content-Type"), Body: body}, nil
}

// ParseTarget validates and normalizes a user-supplied URL.
//
// Bare hostnames are accepted and assumed https, because that is what someone
// pasting from a browser bar will hand us half the time.
func ParseTarget(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("no URL given")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("that does not look like a web address: %w", err)
	}
	if err := checkScheme(parsed); err != nil {
		return nil, err
	}
	if parsed.Host == "" {
		return nil, errors.New("that address has no website in it")
	}
	// A literal IP in the URL is refused outright. There is no legitimate
	// product page at one, and it is the shortest path to the metadata
	// endpoint.
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && !isPublicUnicast(ip) {
		return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
	}
	return parsed, nil
}

func checkScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("only http and https addresses can be read, not %q", u.Scheme)
	}
}

// isPublicUnicast reports whether an address is somewhere on the public
// internet, as opposed to this machine, this network, or the cloud provider's
// own metadata service.
func isPublicUnicast(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsPrivate() {
		return false
	}
	// Carrier-grade NAT and the IPv4 benchmarking range are neither private
	// per RFC 1918 nor anywhere we have business reaching.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1]&0xC0 == 64: // 100.64.0.0/10
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return false
		}
		return true
	}
	// IPv6 unique-local (fc00::/7) is the v6 equivalent of a private range.
	if len(ip) == net.IPv6len && ip[0]&0xFE == 0xFC {
		return false
	}
	return true
}
