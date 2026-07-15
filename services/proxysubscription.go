package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	proxySubscriptionTimeout      = 60 * time.Second
	proxySubscriptionMaxRedirects = 5
	proxySubscriptionMaxNameRunes = 120
	// Base64 expands content by roughly one third. The final generated YAML is
	// still constrained by the regular 4 MiB upload limit.
	proxyMaxSubscriptionResponseSize = proxyMaxConfigSize + proxyMaxConfigSize/3 + 4<<10
)

var errInvalidProxySubscription = errors.New("订阅不是有效的 Clash/Mihomo YAML 或支持的代理 URI 列表")

type proxySubscriptionDownloader func(context.Context, string) (string, error)

// ImportProxySubscriptionForUser downloads a Clash-compatible subscription,
// keeps only its proxy nodes, and publishes it through the normal shared YAML
// upload path. The URL is intentionally not persisted.
func (s *ProxyService) ImportProxySubscriptionForUser(ctx context.Context, subscriptionURL, configName, uploaderID, uploader string) error {
	return s.importProxySubscriptionForUser(ctx, subscriptionURL, configName, uploaderID, uploader, downloadProxySubscription)
}

func (s *ProxyService) importProxySubscriptionForUser(ctx context.Context, subscriptionURL, configName, uploaderID, uploader string, download proxySubscriptionDownloader) error {
	uploaderID = strings.TrimSpace(uploaderID)
	if uploaderID == "" {
		return errors.New("导入代理订阅需要已认证用户")
	}
	if _, err := UserDataDir(uploaderID); err != nil {
		return err
	}
	fileName, err := proxySubscriptionFileName(configName)
	if err != nil {
		return err
	}
	content, err := download(ctx, subscriptionURL)
	if err != nil {
		return err
	}
	nodes, err := parseProxySubscription(content)
	if err != nil {
		return err
	}
	yamlContent, err := yaml.Marshal(map[string]any{"proxies": nodes})
	if err != nil {
		return fmt.Errorf("生成订阅代理配置失败: %w", err)
	}
	return s.UploadProxyConfigForUser(fileName, string(yamlContent), uploaderID, uploader)
}

func proxySubscriptionFileName(configName string) (string, error) {
	name := strings.TrimSpace(configName)
	if name == "" {
		configID, err := newProxyConfigID()
		if err != nil {
			return "", err
		}
		return "subscription-" + configID + ".yaml", nil
	}
	lowerName := strings.ToLower(name)
	if strings.HasSuffix(lowerName, ".yaml") {
		name = strings.TrimSpace(name[:len(name)-len(".yaml")])
	} else if strings.HasSuffix(lowerName, ".yml") {
		name = strings.TrimSpace(name[:len(name)-len(".yml")])
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) || len([]rune(name)) > proxySubscriptionMaxNameRunes {
		return "", fmt.Errorf("订阅名称无效：名称不能为空、不能包含路径分隔符，且不能超过 %d 个字符", proxySubscriptionMaxNameRunes)
	}
	for _, r := range name {
		if r < ' ' || r == '\u007f' {
			return "", errors.New("订阅名称不能包含控制字符")
		}
	}
	return name + ".yaml", nil
}

func downloadProxySubscription(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil || !isHTTPSubscriptionURL(parsed) {
		return "", errors.New("订阅链接必须是有效的 HTTP 或 HTTPS URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	client := newProxySubscriptionHTTPClient(net.DefaultResolver.LookupIP, (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext)
	defer client.CloseIdleConnections()
	return downloadProxySubscriptionWithClient(ctx, parsed, client)
}

type proxySubscriptionLookupIP func(context.Context, string, string) ([]net.IP, error)
type proxySubscriptionDialContext func(context.Context, string, string) (net.Conn, error)

func newProxySubscriptionHTTPClient(lookupIP proxySubscriptionLookupIP, dial proxySubscriptionDialContext) *http.Client {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || baseTransport == nil {
		baseTransport = &http.Transport{}
	}
	transport := baseTransport.Clone()
	// Subscription downloads must connect directly so an environment proxy
	// cannot bypass target validation or gain access to URL credentials.
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		pinnedAddress, err := resolveProxySubscriptionAddress(ctx, address, lookupIP)
		if err != nil {
			return nil, err
		}
		return dial(ctx, network, pinnedAddress)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   proxySubscriptionTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > proxySubscriptionMaxRedirects {
				return fmt.Errorf("订阅重定向次数不能超过 %d 次", proxySubscriptionMaxRedirects)
			}
			if !isHTTPSubscriptionURL(request.URL) {
				return errors.New("订阅重定向到了无效或非 HTTP(S) 地址")
			}
			request.URL.Scheme = strings.ToLower(request.URL.Scheme)
			return nil
		},
	}
}

func resolveProxySubscriptionAddress(ctx context.Context, address string, lookupIP proxySubscriptionLookupIP) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("解析订阅服务器地址失败: %w", err)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "", errors.New("订阅链接不允许访问本机或内网域名")
	}
	var ips []net.IP
	if literal := net.ParseIP(host); literal != nil {
		ips = []net.IP{literal}
	} else {
		ips, err = lookupIP(ctx, "ip", host)
		if err != nil {
			return "", fmt.Errorf("订阅服务器 DNS 解析失败: %w", err)
		}
		if len(ips) == 0 {
			return "", errors.New("订阅服务器 DNS 未返回可用地址")
		}
	}
	for _, ip := range ips {
		if unsafeProxyTestIP(ip) {
			return "", errors.New("订阅链接不允许访问本机、内网或保留地址")
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

func downloadProxySubscriptionWithClient(ctx context.Context, parsed *url.URL, client *http.Client) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("创建订阅下载请求失败: %w", err)
	}
	request.Header.Set("Accept", "text/plain, application/yaml, application/x-yaml, */*")
	request.Header.Set("User-Agent", "ClashMeta")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("下载订阅失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("下载订阅失败: 服务返回 HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(proxyMaxSubscriptionResponseSize)+1))
	if err != nil {
		return "", fmt.Errorf("读取订阅内容失败: %w", err)
	}
	if len(body) > proxyMaxSubscriptionResponseSize {
		return "", fmt.Errorf("订阅内容不能超过 %d MiB", proxyMaxSubscriptionResponseSize>>20)
	}
	if !utf8.Valid(body) {
		return "", errors.New("订阅内容不是 UTF-8 文本")
	}
	return strings.TrimPrefix(string(body), "\ufeff"), nil
}

func isHTTPSubscriptionURL(value *url.URL) bool {
	return value != nil && (strings.EqualFold(value.Scheme, "http") || strings.EqualFold(value.Scheme, "https")) && value.Host != ""
}

func parseProxySubscription(raw string) ([]map[string]any, error) {
	text := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
	if text == "" {
		return nil, errors.New("订阅内容为空")
	}
	if nodes, recognized, err := parseProxySubscriptionYAML(text); recognized {
		return nodes, err
	}
	if nodes, recognized, err := parseProxySubscriptionURIs(text); recognized {
		return nodes, err
	}
	decoded, err := decodeProxySubscriptionBase64(text)
	if err != nil {
		return nil, errInvalidProxySubscription
	}
	if nodes, recognized, parseErr := parseProxySubscriptionYAML(decoded); recognized {
		return nodes, parseErr
	}
	if nodes, recognized, parseErr := parseProxySubscriptionURIs(decoded); recognized {
		return nodes, parseErr
	}
	return nil, errInvalidProxySubscription
}

func parseProxySubscriptionYAML(text string) ([]map[string]any, bool, error) {
	var value map[string]any
	if err := yaml.Unmarshal([]byte(text), &value); err != nil || value == nil {
		return nil, false, nil
	}
	if _, ok := value["proxies"]; !ok {
		return nil, false, nil
	}
	nodes, err := loadProxyYAMLFromBytes([]byte(text), "订阅.yaml")
	return nodes, true, err
}

func parseProxySubscriptionURIs(text string) ([]map[string]any, bool, error) {
	lines := make([]string, 0)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, false, nil
	}
	for _, line := range lines {
		if !strings.Contains(line, "://") {
			return nil, false, nil
		}
	}
	nodes := make([]map[string]any, 0, len(lines))
	for index, line := range lines {
		node, err := convertProxySubscriptionURI(line, index+1)
		if err != nil {
			return nil, true, err
		}
		nodes = append(nodes, node)
	}
	data, err := yaml.Marshal(map[string]any{"proxies": nodes})
	if err != nil {
		return nil, true, err
	}
	validated, err := loadProxyYAMLFromBytes(data, "订阅.yaml")
	return validated, true, err
}

func decodeProxySubscriptionBase64(value string) (string, error) {
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, value)
	if compact == "" {
		return "", errors.New("empty base64")
	}
	compact += strings.Repeat("=", (4-len(compact)%4)%4)
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(compact)
	}
	if err != nil || !utf8.Valid(decoded) {
		return "", errors.New("invalid base64 subscription")
	}
	return strings.TrimSpace(strings.TrimPrefix(string(decoded), "\ufeff")), nil
}

func convertProxySubscriptionURI(raw string, index int) (map[string]any, error) {
	uri, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("第 %d 个代理 URI 无效: %w", index, err)
	}
	scheme := strings.ToLower(uri.Scheme)
	switch scheme {
	case "http", "https", "socks5":
		return convertSimpleProxyURI(uri, index)
	case "ss":
		return convertShadowsocksURI(uri, index)
	case "trojan":
		return convertTrojanURI(uri, index)
	case "vless":
		return convertVLESSURI(uri, index)
	case "vmess":
		return convertVMessURI(raw, index)
	case "hysteria2", "hy2":
		return convertHysteria2URI(uri, index)
	default:
		return nil, fmt.Errorf("第 %d 个代理 URI 使用了不支持的协议 %q", index, scheme)
	}
}

func proxyURIName(uri *url.URL, index int) string {
	if name, err := url.PathUnescape(strings.TrimSpace(uri.Fragment)); err == nil && name != "" {
		return name
	}
	host := uri.Hostname()
	if host == "" {
		host = "proxy"
	}
	if port := uri.Port(); port != "" {
		return host + "-" + port
	}
	return fmt.Sprintf("%s-%d", host, index)
}

func proxyURIServerPort(uri *url.URL, index int) (string, int, error) {
	server := uri.Hostname()
	if server == "" {
		return "", 0, fmt.Errorf("第 %d 个代理 URI 没有服务器地址", index)
	}
	port, err := strconv.Atoi(uri.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("第 %d 个代理 URI 端口无效", index)
	}
	return server, port, nil
}

func proxyURIUser(uri *url.URL) string {
	if uri.User == nil {
		return ""
	}
	return uri.User.Username()
}

func proxyURITransport(query url.Values, network string) map[string]any {
	options := map[string]any{}
	if network != "" {
		options["network"] = network
	}
	switch network {
	case "ws":
		path := query.Get("path")
		if path == "" {
			path = "/"
		}
		ws := map[string]any{"path": path}
		if host := query.Get("host"); host != "" {
			ws["headers"] = map[string]any{"Host": host}
		}
		options["ws-opts"] = ws
	case "grpc":
		if serviceName := query.Get("serviceName"); serviceName != "" {
			options["grpc-opts"] = map[string]any{"grpc-service-name": serviceName}
		}
	}
	return options
}

func convertSimpleProxyURI(uri *url.URL, index int) (map[string]any, error) {
	server, port, err := proxyURIServerPort(uri, index)
	if err != nil {
		return nil, err
	}
	node := map[string]any{"name": proxyURIName(uri, index), "server": server, "port": port, "udp": strings.EqualFold(uri.Scheme, "socks5")}
	if strings.EqualFold(uri.Scheme, "socks5") {
		node["type"] = "socks5"
	} else {
		node["type"] = "http"
	}
	if username := proxyURIUser(uri); username != "" {
		node["username"] = username
	}
	if password, ok := uri.User.Password(); ok {
		node["password"] = password
	}
	return node, nil
}

func convertVLESSURI(uri *url.URL, index int) (map[string]any, error) {
	server, port, err := proxyURIServerPort(uri, index)
	if err != nil {
		return nil, err
	}
	query := uri.Query()
	node := map[string]any{"name": proxyURIName(uri, index), "type": "vless", "server": server, "port": port, "uuid": proxyURIUser(uri), "udp": true}
	for key, value := range proxyURITransport(query, queryValue(query, "type", "tcp")) {
		node[key] = value
	}
	security := query.Get("security")
	if security == "tls" || security == "reality" {
		node["tls"] = true
		node["skip-cert-verify"] = subscriptionBool(query.Get("insecure"), false)
	}
	if value := query.Get("sni"); value != "" {
		node["servername"] = value
	}
	if value := query.Get("fp"); value != "" {
		node["client-fingerprint"] = value
	}
	if value := query.Get("flow"); value != "" {
		node["flow"] = value
	}
	if security == "reality" {
		publicKey, shortID := query.Get("pbk"), query.Get("sid")
		if publicKey == "" || shortID == "" {
			return nil, fmt.Errorf("VLESS Reality 代理 %q 缺少 pbk 或 sid", proxyURIName(uri, index))
		}
		node["reality-opts"] = map[string]any{"public-key": publicKey, "short-id": shortID}
	}
	return node, nil
}

func convertHysteria2URI(uri *url.URL, index int) (map[string]any, error) {
	server, port, err := proxyURIServerPort(uri, index)
	if err != nil {
		return nil, err
	}
	query := uri.Query()
	node := map[string]any{"name": proxyURIName(uri, index), "type": "hysteria2", "server": server, "port": port, "password": proxyURIUser(uri), "udp": true, "skip-cert-verify": subscriptionBool(query.Get("insecure"), false)}
	if value := query.Get("sni"); value != "" {
		node["sni"] = value
	}
	if value := query.Get("mport"); value != "" {
		node["ports"] = value
	}
	if value := query.Get("pinSHA256"); value != "" {
		node["fingerprint"] = value
	}
	return node, nil
}

func convertTrojanURI(uri *url.URL, index int) (map[string]any, error) {
	server, port, err := proxyURIServerPort(uri, index)
	if err != nil {
		return nil, err
	}
	query := uri.Query()
	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	node := map[string]any{"name": proxyURIName(uri, index), "type": "trojan", "server": server, "port": port, "password": proxyURIUser(uri), "udp": true, "tls": true, "skip-cert-verify": subscriptionBool(insecure, false)}
	if value := queryValue(query, "sni", query.Get("peer")); value != "" {
		node["sni"] = value
	}
	for key, value := range proxyURITransport(query, queryValue(query, "type", "tcp")) {
		node[key] = value
	}
	return node, nil
}

func convertVMessURI(raw string, index int) (map[string]any, error) {
	payload := strings.TrimSpace(raw[len("vmess://"):])
	payload = strings.SplitN(payload, "#", 2)[0]
	decoded, err := decodeProxySubscriptionBase64(payload)
	if err != nil {
		return nil, fmt.Errorf("第 %d 个 VMess URI 的 JSON 无效", index)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(decoded), &value); err != nil || value == nil {
		return nil, fmt.Errorf("第 %d 个 VMess URI 的 JSON 无效", index)
	}
	server := strings.TrimSpace(stringValue(value["add"]))
	port, err := strconv.Atoi(stringValue(value["port"]))
	if server == "" || err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("第 %d 个 VMess URI 服务器或端口无效", index)
	}
	name := strings.TrimSpace(stringValue(value["ps"]))
	if name == "" {
		name = fmt.Sprintf("%s-%d", server, port)
	}
	node := map[string]any{"name": name, "type": "vmess", "server": server, "port": port, "uuid": stringValue(value["id"]), "alterId": integerValue(value["aid"]), "cipher": queryValueString(stringValue(value["scy"]), "auto"), "udp": true}
	network := queryValueString(stringValue(value["net"]), "tcp")
	query := url.Values{"host": {stringValue(value["host"])}, "path": {queryValueString(stringValue(value["path"]), "/")}, "serviceName": {stringValue(value["path"])}}
	for key, transportValue := range proxyURITransport(query, network) {
		node[key] = transportValue
	}
	if tlsValue := strings.ToLower(stringValue(value["tls"])); tlsValue == "tls" || tlsValue == "1" || tlsValue == "true" {
		node["tls"] = true
	}
	node["skip-cert-verify"] = subscriptionBool(stringValue(value["allowInsecure"]), false)
	if serverName := queryValueString(stringValue(value["sni"]), stringValue(value["servername"])); serverName != "" {
		node["servername"] = serverName
	}
	if fingerprint := stringValue(value["fp"]); fingerprint != "" {
		node["client-fingerprint"] = fingerprint
	}
	return node, nil
}

func convertShadowsocksURI(uri *url.URL, index int) (map[string]any, error) {
	var cipher, password, server string
	var port int
	var err error
	if uri.User != nil && uri.Hostname() != "" && uri.Port() != "" {
		cipher, password, err = decodeShadowsocksUserinfo(proxyURIUser(uri))
		if err == nil {
			server, port, err = proxyURIServerPort(uri, index)
		}
	} else {
		encoded := uri.Host + uri.Path
		decoded, decodeErr := decodeProxySubscriptionBase64(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("第 %d 个 Shadowsocks URI 无效", index)
		}
		separator := strings.LastIndex(decoded, "@")
		if separator <= 0 || separator == len(decoded)-1 {
			return nil, fmt.Errorf("第 %d 个 Shadowsocks URI 缺少服务器地址", index)
		}
		userinfo, address := decoded[:separator], decoded[separator+1:]
		cipher, password, err = decodeShadowsocksUserinfo(userinfo)
		if err == nil {
			addressURI, parseErr := url.Parse("//" + address)
			if parseErr != nil {
				err = parseErr
			} else {
				server, port, err = proxyURIServerPort(addressURI, index)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("第 %d 个 Shadowsocks URI 无效: %w", index, err)
	}
	return map[string]any{"name": proxyURIName(uri, index), "type": "ss", "server": server, "port": port, "cipher": cipher, "password": password, "udp": true}, nil
}

func decodeShadowsocksUserinfo(value string) (string, string, error) {
	if cipher, password, ok := strings.Cut(value, ":"); ok {
		return cipher, password, nil
	}
	decoded, err := decodeProxySubscriptionBase64(value)
	if err != nil {
		return "", "", errors.New("userinfo 无效")
	}
	cipher, password, ok := strings.Cut(decoded, ":")
	if !ok {
		return "", "", errors.New("userinfo 无效")
	}
	return cipher, password, nil
}

func queryValue(values url.Values, key, fallback string) string {
	if value := values.Get(key); value != "" {
		return value
	}
	return fallback
}

func queryValueString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func subscriptionBool(value string, fallback bool) bool {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func stringValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int:
		return strconv.Itoa(value)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

func integerValue(value any) int {
	parsed, err := strconv.Atoi(stringValue(value))
	if err != nil {
		return 0
	}
	return parsed
}
