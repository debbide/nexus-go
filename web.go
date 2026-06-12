package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
		if strings.Contains(r.URL.Path, "/"+WsPath) && websocket.IsWebSocketUpgrade(r) {
			handleWebSocket(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found\n"))
	})

	mux.HandleFunc("/"+SubPath, handleSubscription)
	mux.HandleFunc("/"+WsPath, func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			handleWebSocket(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found\n"))
	})

	addr := "0.0.0.0:" + PORT
	log.Printf("[INFO] Web server listening on %s", addr)
	server := &http.Server{Addr: addr, Handler: mux}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] Web server failed: %v", err)
	}
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

func handleSubscription(w http.ResponseWriter, r *http.Request) {
	initCurrentISP()
	initCurrentDomain()

	namePart := currentISP
	if NodeName != "" {
		namePart = NodeName + "-" + currentISP
	}

	tlsParam := currentTLS
	ssTLSParam := ""
	if currentTLS == "tls" {
		ssTLSParam = "tls;"
	}
	ssMethodPass := base64.StdEncoding.EncodeToString([]byte("none:" + UUID))

	vlessURL := fmt.Sprintf(
		"vless://%s@%s:%s?encryption=none&security=%s&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
		UUID, currentDomain, currentPort, tlsParam, currentDomain, currentDomain, WsPath, namePart,
	)
	trojanURL := fmt.Sprintf(
		"trojan://%s@%s:%s?security=%s&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
		UUID, currentDomain, currentPort, tlsParam, currentDomain, currentDomain, WsPath, namePart,
	)
	ssURL := fmt.Sprintf(
		"ss://%s@%s:%s?plugin=v2ray-plugin;mode%%3Dwebsocket;host%%3D%s;path%%3D%%2F%s;%ssni%%3D%s;skip-cert-verify%%3Dtrue;mux%%3D0#%s",
		ssMethodPass, currentDomain, currentPort, currentDomain, WsPath, ssTLSParam, currentDomain, namePart,
	)

	subscription := vlessURL + "\n" + trojanURL + "\n" + ssURL
	if CFDomain != "" {
		cfNamePart := namePart + "-CF"
		cfVlessURL := fmt.Sprintf(
			"vless://%s@%s:443?encryption=none&security=tls&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
			UUID, CFDomain, CFDomain, CFDomain, WsPath, cfNamePart,
		)
		cfTrojanURL := fmt.Sprintf(
			"trojan://%s@%s:443?security=tls&sni=%s&fp=chrome&type=ws&host=%s&path=%%2F%s#%s",
			UUID, CFDomain, CFDomain, CFDomain, WsPath, cfNamePart,
		)
		cfSSURL := fmt.Sprintf(
			"ss://%s@%s:443?plugin=v2ray-plugin;mode%%3Dwebsocket;host%%3D%s;path%%3D%%2F%s;tls;sni%%3D%s;skip-cert-verify%%3Dtrue;mux%%3D0#%s",
			ssMethodPass, CFDomain, CFDomain, WsPath, CFDomain, cfNamePart,
		)
		subscription += "\n" + cfVlessURL + "\n" + cfTrojanURL + "\n" + cfSSURL
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(subscription))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(encoded + "\n"))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ERROR] WS Upgrade failed: %v", err)
		return
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, firstMsg, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	go dispatchProxyProtocol(conn, firstMsg)
}
