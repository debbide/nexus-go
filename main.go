package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

var (
	UUID        string
	PORT        string
	CFToken     string
	CFDomain    string
	NezhaServer string
	NezhaPort   string
	NezhaKey    string
	NezhaDoH    string
	SubPath     string
	Domain      string
	WsPath      string
	NodeName    string

	AutoAccess                 bool
	NezhaTLS                   bool
	NezhaReportDelay           int
	NezhaIPReportPeriod        int
	NezhaSkipConnectionCount   bool
	NezhaSkipProcsCount        bool
	NezhaDisableCommandExecute bool
	NezhaDisableSendQuery      bool
	NezhaDisableNat            bool
	NezhaUseIPv6CountryCode    bool
	NezhaDoHEndpoints          []string
	Debug                      bool
)

func initEnv() {
	_ = godotenv.Load()

	UUID = os.Getenv("UUID")
	if UUID == "" {
		UUID = "7bd180e8-1142-4387-93f5-03e8d750a896"
	}

	PORT = os.Getenv("PORT")
	if PORT == "" {
		PORT = os.Getenv("SERVER_PORT")
	}
	if PORT == "" {
		PORT = "3000"
	}

	CFToken = os.Getenv("CF_TUNNEL_TOKEN")
	CFDomain = os.Getenv("CF_DOMAIN")

	NezhaServer = os.Getenv("NEZHA_SERVER")
	NezhaPort = os.Getenv("NEZHA_PORT")
	NezhaKey = os.Getenv("NEZHA_KEY")
	NezhaDoH = os.Getenv("NEZHA_DOH")
	NezhaDoHEndpoints = parseDoHEndpoints(NezhaDoH)

	SubPath = os.Getenv("SUB_PATH")
	if SubPath == "" {
		SubPath = "sub"
	}
	Domain = os.Getenv("DOMAIN")
	AutoAccess = envBool("AUTO_ACCESS", false)

	WsPath = os.Getenv("WSPATH")
	if WsPath == "" && len(UUID) >= 8 {
		WsPath = UUID[:8]
	}
	NodeName = os.Getenv("NAME")
	Debug = envBool("DEBUG", false)

	target := resolveNezhaTarget(NezhaServer, NezhaPort)
	NezhaTLS = envBool("NEZHA_TLS", tlsPorts[extractPort(target)])
	NezhaReportDelay = envInt("NEZHA_REPORT_DELAY", 4)
	if NezhaReportDelay < 1 {
		NezhaReportDelay = 1
	}
	if NezhaReportDelay > 4 {
		NezhaReportDelay = 4
	}
	NezhaIPReportPeriod = envInt("NEZHA_IP_REPORT_PERIOD", 1800)
	if NezhaIPReportPeriod < 30 {
		NezhaIPReportPeriod = 30
	}
	NezhaSkipConnectionCount = envBool("NEZHA_SKIP_CONNECTION_COUNT", true)
	NezhaSkipProcsCount = envBool("NEZHA_SKIP_PROCS_COUNT", true)
	NezhaDisableCommandExecute = envBool("NEZHA_DISABLE_COMMAND_EXECUTE", false)
	NezhaDisableSendQuery = envBool("NEZHA_DISABLE_SEND_QUERY", false)
	NezhaDisableNat = envBool("NEZHA_DISABLE_NAT", true)
	NezhaUseIPv6CountryCode = envBool("NEZHA_USE_IPV6_COUNTRY_CODE", false)

	configureLogging()
	log.Printf("[INFO] Configuration loaded. PORT=%s, SUB_PATH=/%s, WSPATH=/%s", PORT, SubPath, WsPath)
}

func configureLogging() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if !Debug {
		log.SetOutput(io.Discard)
	}
}

func preparePort() {
	start, err := strconv.Atoi(PORT)
	if err != nil || isPortAvailable(PORT) {
		return
	}
	log.Printf("[WARN] Port %s is already in use, finding available port...", PORT)
	next := findAvailablePort(start+1, 100)
	if next == "" {
		log.Fatalf("[FATAL] No available ports found")
	}
	log.Printf("[INFO] Using port %s instead of %s", next, PORT)
	PORT = next
}

func addAccessTask() {
	if !AutoAccess || Domain == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"url": "https://" + Domain + "/" + SubPath})
	req, err := http.NewRequest("POST", "https://oooo.serv00.net/add-url", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
	log.Printf("[INFO] Automatic Access Task added successfully")
}

func main() {
	initEnv()
	log.Println("[INFO] OneImg Go Native Node Starting...")
	preparePort()

	go startWebServer()

	if NezhaServer != "" && NezhaKey != "" {
		go startNezhaAgent()
	}

	go addAccessTask()

	if CFToken != "" {
		go startCFTunnel()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("[INFO] Shutting down...")
}
