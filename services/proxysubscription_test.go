package services

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseProxySubscription(t *testing.T) {
	vmessPayload := base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"vmess-node","add":"vmess.example","port":"443","id":"00000000-0000-0000-0000-000000000000","aid":"0","net":"ws","tls":"tls"}`))
	ssPayload := base64.RawStdEncoding.EncodeToString([]byte("aes-128-gcm:password@ss.example:8388"))
	uriList := strings.Join([]string{
		"https://user:pass@http.example:443#http-node",
		"socks5://user:pass@socks.example:1080#socks-node",
		"ss://" + ssPayload + "#ss-node",
		"trojan://secret@trojan.example:443?type=ws&host=cdn.example#trojan-node",
		"vless://00000000-0000-0000-0000-000000000000@vless.example:443?security=reality&pbk=key&sid=short&type=grpc#vless-node",
		"hysteria2://secret@hy.example:443?sni=hy.example#hy-node",
		"vmess://" + vmessPayload,
	}, "\n")

	t.Run("Clash YAML", func(t *testing.T) {
		nodes, err := parseProxySubscription("proxies:\n  - name: yaml-node\n    type: http\n    server: yaml.example\n    port: 443\n")
		if err != nil || len(nodes) != 1 || nodes[0]["name"] != "yaml-node" {
			t.Fatalf("nodes = %#v, err = %v", nodes, err)
		}
	})

	t.Run("plain URI list", func(t *testing.T) {
		nodes, err := parseProxySubscription(uriList)
		if err != nil || len(nodes) != 7 {
			t.Fatalf("nodes = %#v, err = %v", nodes, err)
		}
		if nodes[2]["type"] != "ss" || nodes[6]["type"] != "vmess" {
			t.Fatalf("converted types = %#v", nodes)
		}
	})

	t.Run("Base64 URI list", func(t *testing.T) {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(uriList))
		nodes, err := parseProxySubscription(encoded)
		if err != nil || len(nodes) != 7 {
			t.Fatalf("nodes = %#v, err = %v", nodes, err)
		}
	})
}

func TestParseProxySubscriptionProtocolEdgeCases(t *testing.T) {
	t.Run("VMess boolean TLS fields", func(t *testing.T) {
		payload := base64.RawStdEncoding.EncodeToString([]byte(`{"v":"2","ps":"bool-tls","add":"vmess.example","port":443,"id":"00000000-0000-0000-0000-000000000000","aid":0,"tls":true,"allowInsecure":true}`))
		nodes, err := parseProxySubscription("vmess://" + payload)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("nodes = %#v, err = %v", nodes, err)
		}
		if nodes[0]["tls"] != true || nodes[0]["skip-cert-verify"] != true {
			t.Fatalf("VMess TLS fields = %#v", nodes[0])
		}
	})

	t.Run("Shadowsocks password containing at sign", func(t *testing.T) {
		payload := base64.RawStdEncoding.EncodeToString([]byte("aes-128-gcm:p@ss@ss.example:8388"))
		nodes, err := parseProxySubscription("ss://" + payload + "#ss-at")
		if err != nil || len(nodes) != 1 {
			t.Fatalf("nodes = %#v, err = %v", nodes, err)
		}
		if nodes[0]["password"] != "p@ss" || nodes[0]["server"] != "ss.example" {
			t.Fatalf("Shadowsocks node = %#v", nodes[0])
		}
	})
}

func TestParseProxySubscriptionRejectsInvalidInputs(t *testing.T) {
	for _, content := range []string{
		"",
		"ftp://example.com:21",
		"proxies: not-a-list",
		"https://missing-port.example#broken",
	} {
		if _, err := parseProxySubscription(content); err == nil {
			t.Fatalf("invalid subscription accepted: %q", content)
		}
	}
}

func TestImportProxySubscriptionForUser(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake Mihomo validation test requires a POSIX shell")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODE_SWITCH_REFERENCE_PROXY_DIR", t.TempDir())
	fakeMihomo := filepath.Join(t.TempDir(), "mihomo")
	script := "#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then echo 'Mihomo Meta v1.19.28'; fi\nexit 0\n"
	if err := os.WriteFile(fakeMihomo, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODE_SWITCH_MIHOMO_BINARY", fakeMihomo)
	service, err := NewProxyService()
	if err != nil {
		t.Fatalf("NewProxyService: %v", err)
	}
	t.Cleanup(service.Stop)

	downloadCalls := 0
	download := func(_ context.Context, rawURL string) (string, error) {
		downloadCalls++
		if rawURL != "https://subscription.example/token" {
			t.Fatalf("subscription URL = %q", rawURL)
		}
		return "proxies:\n  - name: imported\n    type: http\n    server: proxy.example\n    port: 443\n", nil
	}
	if err := service.importProxySubscriptionForUser(context.Background(), "https://subscription.example/token", "My Subscription.yaml", "alice-id", "alice", download); err != nil {
		t.Fatalf("importProxySubscriptionForUser: %v", err)
	}
	configs, err := service.ListProxyConfigsForUser("alice-id")
	if err != nil || len(configs) != 1 || !configs[0].IsOwner || configs[0].FileName != "My Subscription.yaml" {
		t.Fatalf("configs = %#v, err = %v", configs, err)
	}
	if downloadCalls != 1 {
		t.Fatalf("download calls = %d", downloadCalls)
	}

	downloadCalls = 0
	if err := service.importProxySubscriptionForUser(context.Background(), "https://subscription.example/token", "anonymous", "", "anonymous", download); err == nil {
		t.Fatal("unauthenticated subscription import was accepted")
	}
	if downloadCalls != 0 {
		t.Fatal("unauthenticated import performed a network request")
	}
}

func TestProxySubscriptionFileName(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain name", input: "Hong Kong Nodes", want: "Hong Kong Nodes.yaml"},
		{name: "yaml extension", input: "Hong Kong Nodes.YAML", want: "Hong Kong Nodes.yaml"},
		{name: "yml extension", input: "Hong Kong Nodes.yml", want: "Hong Kong Nodes.yaml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := proxySubscriptionFileName(test.input)
			if err != nil || got != test.want {
				t.Fatalf("file name = %q, err = %v", got, err)
			}
		})
	}
	generated, err := proxySubscriptionFileName("  ")
	if err != nil || !strings.HasPrefix(generated, "subscription-") || !strings.HasSuffix(generated, ".yaml") {
		t.Fatalf("generated file name = %q, err = %v", generated, err)
	}
	for _, invalid := range []string{"../secret", `folder\\secret`, ".yaml", "bad\nname", strings.Repeat("长", proxySubscriptionMaxNameRunes+1)} {
		if _, err := proxySubscriptionFileName(invalid); err == nil {
			t.Fatalf("invalid subscription name accepted: %q", invalid)
		}
	}
}

func TestDownloadProxySubscription(t *testing.T) {
	t.Run("uses Clash headers and follows HTTP redirects", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/subscription", http.StatusFound)
				return
			}
			if got := r.Header.Get("User-Agent"); got != "ClashMeta" {
				t.Errorf("User-Agent = %q", got)
			}
			_, _ = w.Write([]byte("proxies: []"))
		}))
		defer server.Close()

		content, err := downloadProxySubscriptionForTest(context.Background(), server, "/start")
		if err != nil || content != "proxies: []" {
			t.Fatalf("content = %q, err = %v", content, err)
		}
	})

	t.Run("rejects non-HTTP URL", func(t *testing.T) {
		if _, err := downloadProxySubscription(context.Background(), "file:///tmp/subscription"); err == nil {
			t.Fatal("file URL accepted")
		}
	})

	t.Run("rejects private and reserved targets", func(t *testing.T) {
		for _, rawURL := range []string{
			"http://127.0.0.1/subscription",
			"http://[::1]/subscription",
			"http://169.254.169.254/latest/meta-data",
			"http://192.0.2.1/subscription",
			"http://localhost/subscription",
		} {
			if _, err := downloadProxySubscription(context.Background(), rawURL); err == nil {
				t.Fatalf("unsafe subscription target accepted: %s", rawURL)
			}
		}
	})

	t.Run("rejects private redirect target", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
		}))
		defer server.Close()
		if _, err := downloadProxySubscriptionForTest(context.Background(), server, "/start"); err == nil || !strings.Contains(err.Error(), "不允许") {
			t.Fatalf("private redirect error = %v", err)
		}
	})

	t.Run("limits redirects", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			http.Redirect(w, r, "/loop", http.StatusFound)
		}))
		defer server.Close()
		if _, err := downloadProxySubscriptionForTest(context.Background(), server, "/loop"); err == nil || !strings.Contains(err.Error(), "重定向次数") {
			t.Fatalf("redirect limit error = %v", err)
		}
		if requests != proxySubscriptionMaxRedirects+1 {
			t.Fatalf("redirect requests = %d, want %d", requests, proxySubscriptionMaxRedirects+1)
		}
	})

	t.Run("rejects oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", proxyMaxSubscriptionResponseSize+1)))
		}))
		defer server.Close()
		if _, err := downloadProxySubscriptionForTest(context.Background(), server, "/"); err == nil {
			t.Fatal("oversized subscription accepted")
		}
	})
}

func TestResolveProxySubscriptionAddressPinsPublicDNS(t *testing.T) {
	lookupCalls := 0
	lookupIP := func(_ context.Context, network, host string) ([]net.IP, error) {
		lookupCalls++
		if network != "ip" || host != "subscription.example" {
			t.Fatalf("unexpected lookup: network=%s host=%s", network, host)
		}
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")}, nil
	}
	pinned, err := resolveProxySubscriptionAddress(context.Background(), "subscription.example:8443", lookupIP)
	if err != nil || pinned != "8.8.8.8:8443" {
		t.Fatalf("pinned address = %q, err = %v", pinned, err)
	}
	if lookupCalls != 1 {
		t.Fatalf("lookup calls = %d", lookupCalls)
	}
}

func TestResolveProxySubscriptionAddressRejectsMixedDNSAnswers(t *testing.T) {
	lookupIP := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}, nil
	}
	if _, err := resolveProxySubscriptionAddress(context.Background(), "subscription.example:443", lookupIP); err == nil {
		t.Fatal("mixed public/private DNS answers were accepted")
	}
}

func downloadProxySubscriptionForTest(ctx context.Context, server *httptest.Server, path string) (string, error) {
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		return "", err
	}
	serverAddress := server.Listener.Addr().String()
	lookupIP := func(_ context.Context, network, host string) ([]net.IP, error) {
		if network != "ip" || host != "subscription.example" {
			return nil, fmt.Errorf("unexpected lookup: network=%s host=%s", network, host)
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
	}
	client := newProxySubscriptionHTTPClient(lookupIP, dial)
	defer client.CloseIdleConnections()
	parsed := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("subscription.example", serverURL.Port()),
		Path:   path,
	}
	return downloadProxySubscriptionWithClient(ctx, parsed, client)
}
