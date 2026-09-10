package utils

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// NewHTTPClient returns a client bounded by timeout that sends every request
// through proxy, or makes them directly when proxy is empty.
//
// The proxy applies to this client alone, which is the point: it lets one
// source (Reddit) leave through a different network than everything else the
// bot talks to, without an HTTP_PROXY variable that would reroute Discord,
// YouTube and the database-adjacent services along with it.
//
// Accepted schemes are the ones net/http dials itself: http, https, socks5 and
// socks5h. Anything else is refused here rather than failing on the first
// request, where it would look like the upstream being down.
func NewHTTPClient(timeout time.Duration, proxy string) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	if proxy == "" {
		return client, nil
	}

	u, err := url.Parse(proxy)
	if err != nil {
		// url.Error repeats the raw URL, which may carry proxy credentials.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("proxy is not a valid URL: %w", err)
	}

	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q, want http, https, socks5 or socks5h", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q has no host", u.Redacted())
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(u)
	client.Transport = transport
	return client, nil
}
