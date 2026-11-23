package podcast

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// MaxFeedBytes is the largest feed body we will read.
const MaxFeedBytes = 10 << 20

// ErrBlockedAddress is returned when a feed URL resolves to an address we
// refuse to connect to (loopback, private networks, cloud metadata, ...).
var ErrBlockedAddress = errors.New("podcast: address is not allowed")

// Fetcher downloads feeds. Users choose the URLs, so the connection itself is
// guarded: the check runs on the address the dialer is about to connect to, which
// also covers redirects and DNS answers that change between lookup and connect.
type Fetcher struct {
	client *http.Client
}

// NewFetcher creates a Fetcher. allowPrivate disables the address guard; use it
// only in development and tests.
func NewFetcher(allowPrivate bool, timeout time.Duration) *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlockedAddress
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route through an environment proxy: it would bypass the guard
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	}
	return &Fetcher{client: &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to a non-http URL")
			}
			return nil
		},
	}}
}

// Result is the outcome of one fetch.
type Result struct {
	Body         []byte // nil when NotModified
	NotModified  bool
	ETag         string
	LastModified string
}

// Fetch downloads a feed, sending the previous validators so unchanged feeds
// cost one tiny request. Errors are *FeedError (safe to show) or system errors.
func (f *Fetcher) Fetch(ctx context.Context, feedURL, etag, lastModified string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, &FeedError{"feedUrl is not a valid URL"}
	}
	req.Header.Set("User-Agent", "BlacMusic-FeedFetcher/1.0")
	req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.8, */*;q=0.5")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return nil, &FeedError{"that address cannot be fetched"}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &FeedError{"the feed could not be reached"}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return &Result{NotModified: true, ETag: etag, LastModified: lastModified}, nil
	case resp.StatusCode != http.StatusOK:
		return nil, &FeedError{fmt.Sprintf("the feed server answered HTTP %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFeedBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &FeedError{"the feed download was interrupted"}
	}
	if len(body) > MaxFeedBytes {
		return nil, &FeedError{"the feed is too large"}
	}
	return &Result{
		Body:         body,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

var blockedNets = mustCIDRs(
	"0.0.0.0/8",       // "this" network
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"240.0.0.0/4",     // reserved
	"64:ff9b::/96",    // NAT64 (can embed private IPv4)
	"2002::/16",       // 6to4 (can embed private IPv4)
)

func mustCIDRs(list ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(list))
	for i, c := range list {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out[i] = n
	}
	return out
}

// blockedIP reports whether we refuse to connect to ip.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
