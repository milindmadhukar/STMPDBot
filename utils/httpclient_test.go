package utils_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/milindmadhukar/STMPDBot/utils"
)

func TestNewHTTPClient_WithoutAProxyGoesDirect(t *testing.T) {
	t.Parallel()

	client, err := utils.NewHTTPClient(5*time.Second, "")
	if err != nil {
		t.Fatalf("NewHTTPClient returned an error: %v", err)
	}
	if client.Transport != nil {
		t.Errorf("Transport = %T, want nil so the client behaves like any other", client.Transport)
	}
	if client.Timeout != 5*time.Second {
		t.Errorf("Timeout = %s, want 5s", client.Timeout)
	}
}

// A plain-http proxy receives the absolute URL of every request, so a fake one
// that answers for a host that does not exist proves the request went through
// it rather than being dialled directly.
func TestNewHTTPClient_SendsRequestsThroughTheProxy(t *testing.T) {
	t.Parallel()

	var sawHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHost = r.URL.Host
		_, _ = io.WriteString(w, "via proxy")
	}))
	t.Cleanup(proxy.Close)

	client, err := utils.NewHTTPClient(5*time.Second, proxy.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient returned an error: %v", err)
	}

	resp, err := client.Get("http://reddit.invalid/r/Martingarrix/new")
	if err != nil {
		t.Fatalf("request through the proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via proxy" {
		t.Errorf("body = %q, want the proxy's answer", body)
	}
	if sawHost != "reddit.invalid" {
		t.Errorf("proxy saw host %q, want reddit.invalid", sawHost)
	}
}

func TestNewHTTPClient_AcceptsEverySchemeNetHTTPDials(t *testing.T) {
	t.Parallel()

	for _, proxy := range []string{
		"http://tailscale-raspberrypi:1080",
		"https://proxy.example.com:443",
		"socks5://tailscale-raspberrypi:1055",
		"socks5h://tailscale-raspberrypi:1055",
	} {
		if _, err := utils.NewHTTPClient(time.Second, proxy); err != nil {
			t.Errorf("NewHTTPClient(%q) returned %v, want it accepted", proxy, err)
		}
	}
}

func TestNewHTTPClient_RefusesAProxyItCannotUse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		proxy string
	}{
		// Parses as scheme "tailscale-raspberrypi", which is the easiest
		// mistake to make and the hardest to spot in a failing request.
		{"a missing scheme", "tailscale-raspberrypi:1080"},
		{"an unsupported scheme", "ftp://tailscale-raspberrypi:1080"},
		{"no host", "http://"},
		{"not a URL at all", "http://[::1"},
	} {
		if _, err := utils.NewHTTPClient(time.Second, tc.proxy); err == nil {
			t.Errorf("%s: NewHTTPClient(%q) returned no error", tc.name, tc.proxy)
		}
	}
}

// The error ends up in the bot's log, so credentials in the proxy URL must not.
func TestNewHTTPClient_DoesNotLogProxyCredentials(t *testing.T) {
	t.Parallel()

	for _, proxy := range []string{
		"http://user:hunter2@",
		"http://user:hunter2@[::1",
	} {
		_, err := utils.NewHTTPClient(time.Second, proxy)
		if err == nil {
			t.Fatalf("NewHTTPClient(%q) returned no error", proxy)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error %q contains the proxy password", err)
		}
	}
}
