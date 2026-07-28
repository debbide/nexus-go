package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed index.html
var embeddedIndex []byte

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var (
	currentDomain string
	currentPort   string
	currentTLS    string
	currentISP    string
)

func startWebServer() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			serveIndex(w, r)
			return
		}
		if r.URL.Path == "/"+SubPath {
			handleSubscription(w, r)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			if handleAnyVLESSWebSocket(w, r) {
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found\n"))
	})

	mux.HandleFunc("/"+SubPath, handleSubscription)
	mux.HandleFunc("/"+WsPath, func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			handleWebSocketTo(w, r, singBoxVLESSListenPort, singBoxVLESSPath())
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found\n"))
	})
	for _, ex := range getFanoutExits() {
		path := trimPath(ex.Path)
		port := ex.ListenPort
		wsPath := "/" + path
		mux.HandleFunc("/"+path, func(w http.ResponseWriter, r *http.Request) {
			if websocket.IsWebSocketUpgrade(r) {
				handleWebSocketTo(w, r, port, wsPath)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("Not Found\n"))
		})
	}

	addr := "0.0.0.0:" + PORT
	log.Printf("[INFO] Web server listening on %s", addr)
	server := &http.Server{Addr: addr, Handler: mux}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] Web server failed: %v", err)
	}
}

// handleAnyVLESSWebSocket 按 path 分到主入口或 fanout 旁路入口。
func handleAnyVLESSWebSocket(w http.ResponseWriter, r *http.Request) bool {
	reqPath := trimPath(r.URL.Path)
	if reqPath == trimPath(WsPath) || strings.Contains(r.URL.Path, "/"+trimPath(WsPath)) {
		handleWebSocketTo(w, r, singBoxVLESSListenPort, singBoxVLESSPath())
		return true
	}
	if ex, ok := findFanoutByPath(reqPath); ok && ex.ListenPort > 0 {
		handleWebSocketTo(w, r, ex.ListenPort, "/"+trimPath(ex.Path))
		return true
	}
	return false
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	for _, path := range []string{"index.html", "../index.html"} {
		if _, err := os.Stat(path); err == nil {
			http.ServeFile(w, r, path)
			return
		}
	}
	if len(embeddedIndex) > 0 {
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(embeddedIndex))
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte("Hello world!"))
}

func initCurrentDomain() {
	if Domain == "" || Domain == "your-domain.com" {
		ip := fetchPublicIPv4()
		if ip != "" {
			currentDomain = ip
			currentPort = PORT
			currentTLS = "none"
			return
		}
		currentDomain = "change-your-domain.com"
		currentPort = "443"
		currentTLS = "tls"
		return
	}
	currentDomain = Domain
	currentPort = "443"
	currentTLS = "tls"
}

func initCurrentISP() {
	client := &http.Client{Timeout: 3 * time.Second}

	req, _ := http.NewRequest("GET", "https://api.ip.sb/geoip", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if resp, err := client.Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var data map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
				currentISP = strings.ReplaceAll(jsonString(data, "country_code")+"-"+jsonString(data, "isp"), " ", "_")
				return
			}
		}
	}

	req2, _ := http.NewRequest("GET", "http://ip-api.com/json", nil)
	req2.Header.Set("User-Agent", "Mozilla/5.0")
	if resp, err := client.Do(req2); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var data map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
				currentISP = strings.ReplaceAll(jsonString(data, "countryCode")+"-"+jsonString(data, "org"), " ", "_")
				return
			}
		}
	}

	currentISP = "Unknown"
}

func jsonString(data map[string]interface{}, key string) string {
	if value, ok := data[key]; ok && value != nil {
		return fmt.Sprintf("%v", value)
	}
	return ""
}

func fetchPublicIPv4() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api-ipv4.ip.sb/ip")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func fetchPublicIPv6() string {
	client := &http.Client{Timeout: 5 * time.Second}
	// 多个源兜底
	for _, u := range []string{
		"https://api-ipv6.ip.sb/ip",
		"https://api6.ipify.org",
		"https://v6.ident.me",
	} {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil && strings.Contains(ip, ":") {
			return ip
		}
	}
	return ""
}

// formatURIHost 把 IPv6 收成 [addr]，域名/IPv4 原样，用于 URI host 段
func formatURIHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return host
	}
	if strings.HasPrefix(host, "[") {
		return host
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

func handleSubscription(w http.ResponseWriter, r *http.Request) {
	initCurrentISP()
	initCurrentDomain()

	namePart := currentISP
	if NodeName != "" {
		namePart = NodeName + "-" + currentISP
	}

	tlsParam := currentTLS
	vlessURL := fmt.Sprintf(
		"vless://%s@%s:%s?encryption=none&security=%s&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
		UUID, currentDomain, currentPort, tlsParam, currentDomain, currentDomain, trimPath(WsPath), namePart+"-VLESS",
	)
	subscription := vlessURL
	if TUICPort != "" && TUICPort != "0" {
		tuicURL := buildTUICURL(namePart + "-TUIC")
		subscription += "\n" + tuicURL
	}
	if HY2Port != "" && HY2Port != "0" {
		hy2URL := buildHY2URL(namePart + "-HY2")
		subscription += "\n" + hy2URL
	}

	if CFDomain != "" {
		cfNamePart := namePart + "-CF"
		cfVlessURL := fmt.Sprintf(
			"vless://%s@%s:443?encryption=none&security=tls&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
			UUID, CFDomain, CFDomain, CFDomain, trimPath(WsPath), cfNamePart+"-VLESS",
		)
		subscription += "\n" + cfVlessURL
	}

	// fanout 旁路节点：一国一条，走独立 path → 对应 SOCKS 出口；主节点不受影响
	for _, ex := range getFanoutExits() {
		if ex.ListenPort == 0 {
			continue
		}
		foName := namePart + "-" + strings.ToUpper(ex.Code) + "-Fanout"
		// 直连（IP/域名 + 本机 Web 端口）
		subscription += "\n" + fmt.Sprintf(
			"vless://%s@%s:%s?encryption=none&security=%s&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
			UUID, currentDomain, currentPort, tlsParam, currentDomain, currentDomain, trimPath(ex.Path), foName,
		)
		// CF 域名（与主站同 443，不同 path）
		if CFDomain != "" {
			subscription += "\n" + fmt.Sprintf(
				"vless://%s@%s:443?encryption=none&security=tls&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
				UUID, CFDomain, CFDomain, CFDomain, trimPath(ex.Path), foName+"-CF",
			)
		}
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(subscription))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(encoded + "\n"))
}

func buildTUICURL(name string) string {
	host := resolveTUICServerName()
	query := url.Values{
		"allow_insecure":     {"1"},
		"alpn":               {"h3"},
		"congestion_control": {"bbr"},
		"insecure":           {"1"},
		"skip-cert-verify":   {"true"},
		"sni":                {host},
		"udp_relay_mode":     {"native"},
		"version":            {"5"},
	}.Encode()
	return fmt.Sprintf(
		"tuic://%s:%s@%s:%s?%s#%s",
		url.QueryEscape(UUID),
		url.QueryEscape(TUICPassword),
		formatURIHost(host),
		TUICPort,
		query,
		url.QueryEscape(name),
	)
}

func buildHY2URL(name string) string {
	host := resolveHY2ServerName()
	query := url.Values{
		"insecure": {"1"},
		"sni":      {host},
		"alpn":     {"h3"},
	}
	if HY2ObfsPass != "" {
		query.Set("obfs", "salamander")
		query.Set("obfs-password", HY2ObfsPass)
	}
	return fmt.Sprintf(
		"hysteria2://%s@%s:%s?%s#%s",
		url.QueryEscape(HY2Password),
		formatURIHost(host),
		HY2Port,
		query.Encode(),
		url.QueryEscape(name),
	)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	handleWebSocketTo(w, r, singBoxVLESSListenPort, singBoxVLESSPath())
}

func handleWebSocketTo(w http.ResponseWriter, r *http.Request, backendPort uint16, backendPath string) {
	if backendPort == 0 {
		log.Printf("[ERROR] VLESS backend port not ready for path %s", backendPath)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if !strings.HasPrefix(backendPath, "/") {
		backendPath = "/" + backendPath
	}
	targetURL := "ws://127.0.0.1:" + strconv.Itoa(int(backendPort)) + backendPath
	targetHeader := http.Header{}
	for _, protocol := range r.Header.Values("Sec-WebSocket-Protocol") {
		targetHeader.Add("Sec-WebSocket-Protocol", protocol)
	}
	// 上传测速会灌大帧；放大缓冲，避免默认 4KiB 读缓冲导致频繁 syscall
	dialer := *websocket.DefaultDialer
	dialer.ReadBufferSize = 64 * 1024
	dialer.WriteBufferSize = 64 * 1024
	backend, _, err := dialer.Dial(targetURL, targetHeader)
	if err != nil {
		log.Printf("[ERROR] sing-box VLESS dial failed (%s): %v", targetURL, err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer backend.Close()

	upgraderLocal := upgrader
	upgraderLocal.ReadBufferSize = 64 * 1024
	upgraderLocal.WriteBufferSize = 64 * 1024
	upgraderLocal.EnableCompression = false
	conn, err := upgraderLocal.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ERROR] WS Upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// 双向桥接：一侧结束后关闭对端写，再等另一侧，避免上传测速被下载侧抢先整链拆掉
	done := make(chan struct{}, 2)
	go copyWebSocketMessages(conn, backend, done)
	go copyWebSocketMessages(backend, conn, done)
	<-done
	_ = conn.Close()
	_ = backend.Close()
	<-done
}

func copyWebSocketMessages(dst, src *websocket.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	src.SetReadLimit(8 << 20) // 8MiB，覆盖测速大帧
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			// 半关闭：通知对端本方向结束，让另一侧 copy 也能尽快退出
			_ = dst.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(2*time.Second))
			return
		}
		if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
			continue
		}
		if err := dst.WriteMessage(messageType, payload); err != nil {
			return
		}
	}
}
