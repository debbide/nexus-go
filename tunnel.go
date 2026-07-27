package main

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"
)

type capnpMessage struct {
	words []uint64
}

func (m *capnpMessage) allocate(wordCount int) int {
	offset := len(m.words)
	m.words = append(m.words, make([]uint64, wordCount)...)
	return offset
}

func (m *capnpMessage) setStructPointer(ptrWordOffset, targetWordOffset, dataWords, pointerWords int) {
	offset := uint64(targetWordOffset - ptrWordOffset - 1)
	low := (offset << 2) & 0xFFFFFFFC
	high := uint64((dataWords & 0xFFFF) | ((pointerWords & 0xFFFF) << 16))
	m.words[ptrWordOffset] = (low & 0xFFFFFFFF) | (high << 32)
}

func (m *capnpMessage) setUint16(wordOffset, byteIndex int, value uint16) {
	word := m.words[wordOffset]
	mask := ^(uint64(0xFFFF) << (byteIndex * 8))
	word = (word & mask) | (uint64(value&0xFFFF) << (byteIndex * 8))
	m.words[wordOffset] = word
}

func (m *capnpMessage) setUint32(wordOffset, byteIndex int, value uint32) {
	word := m.words[wordOffset]
	mask := ^(uint64(0xFFFFFFFF) << (byteIndex * 8))
	word = (word & mask) | (uint64(value) << (byteIndex * 8))
	m.words[wordOffset] = word
}

func (m *capnpMessage) setUint8(wordOffset, byteIndex int, value uint8) {
	word := m.words[wordOffset]
	mask := ^(uint64(0xFF) << (byteIndex * 8))
	word = (word & mask) | (uint64(value) << (byteIndex * 8))
	m.words[wordOffset] = word
}

func (m *capnpMessage) setUint64(wordOffset int, value uint64) {
	m.words[wordOffset] = value
}

func (m *capnpMessage) writeText(ptrWordOffset int, text string) int {
	utf8 := []byte(text)
	byteCount := len(utf8) + 1
	wordCount := (byteCount + 7) / 8
	contentOffset := m.allocate(wordCount)
	for i, b := range utf8 {
		m.setUint8(contentOffset+i/8, i%8, b)
	}
	offset := uint64(contentOffset - ptrWordOffset - 1)
	low := ((offset << 2) | 1) & 0xFFFFFFFF
	high := uint64(2 | ((byteCount & 0x1FFFFFFF) << 3))
	m.words[ptrWordOffset] = (low & 0xFFFFFFFF) | (high << 32)
	return contentOffset
}

func (m *capnpMessage) writeData(ptrWordOffset int, data []byte) int {
	byteCount := len(data)
	wordCount := (byteCount + 7) / 8
	contentOffset := m.allocate(wordCount)
	for i, b := range data {
		m.setUint8(contentOffset+i/8, i%8, b)
	}
	offset := uint64(contentOffset - ptrWordOffset - 1)
	low := ((offset << 2) | 1) & 0xFFFFFFFF
	high := uint64(2 | ((byteCount & 0x1FFFFFFF) << 3))
	m.words[ptrWordOffset] = (low & 0xFFFFFFFF) | (high << 32)
	return contentOffset
}

func (m *capnpMessage) writeTextList(ptrWordOffset int, texts []string) int {
	if len(texts) == 0 {
		m.words[ptrWordOffset] = 0
		return -1
	}
	listOffset := m.allocate(len(texts))
	offset := uint64(listOffset - ptrWordOffset - 1)
	low := ((offset << 2) | 1) & 0xFFFFFFFF
	high := uint64(6 | ((len(texts) & 0x1FFFFFFF) << 3))
	m.words[ptrWordOffset] = (low & 0xFFFFFFFF) | (high << 32)
	for i, text := range texts {
		m.writeText(listOffset+i, text)
	}
	return listOffset
}

func (m *capnpMessage) toBytes() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(0))
	binary.Write(buf, binary.LittleEndian, uint32(len(m.words)))
	for _, word := range m.words {
		binary.Write(buf, binary.LittleEndian, word)
	}
	return buf.Bytes()
}

func capnpBootstrap(questionID uint32) []byte {
	msg := &capnpMessage{}
	rootPtr, msgData, msgPtr := msg.allocate(1), msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(rootPtr, msgData, 1, 1)
	msg.setUint16(msgData, 0, 8) // MSG_BOOTSTRAP
	bsData := msg.allocate(1)
	msg.allocate(1)
	msg.setStructPointer(msgPtr, bsData, 1, 1)
	msg.setUint32(bsData, 0, questionID)
	return msg.toBytes()
}

func capnpRegisterConnection(questionID, bsQuestionID uint32, accountTag string, tunnelSecret, tunnelID []byte, connIndex uint8, clientID []byte) []byte {
	msg := &capnpMessage{}
	rootPtr, msgData, msgPtr := msg.allocate(1), msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(rootPtr, msgData, 1, 1)
	msg.setUint16(msgData, 0, 2) // MSG_CALL
	callData0, callData1, _ := msg.allocate(1), msg.allocate(1), msg.allocate(1)
	callPtr0, callPtr1, _ := msg.allocate(1), msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(msgPtr, callData0, 3, 3)
	msg.setUint32(callData0, 0, questionID)
	msg.setUint16(callData0, 4, 0)
	msg.setUint16(callData0, 6, 0)
	msg.setUint64(callData1, 0xf71695ec7fe85497) // REGISTRATION_SERVER_ID
	mtData, mtPtr := msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(callPtr0, mtData, 1, 1)
	msg.setUint16(mtData, 4, 1)
	paData := msg.allocate(1)
	msg.allocate(1)
	msg.setStructPointer(mtPtr, paData, 1, 1)
	msg.setUint32(paData, 0, bsQuestionID)
	payloadPtr0, _ := msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(callPtr1, payloadPtr0, 0, 2)
	paramsData, paramsPtr0, paramsPtr1, paramsPtr2 := msg.allocate(1), msg.allocate(1), msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(payloadPtr0, paramsData, 1, 3)
	msg.setUint8(paramsData, 0, connIndex)
	authPtr0, authPtr1 := msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(paramsPtr0, authPtr0, 0, 2)
	msg.writeText(authPtr0, accountTag)
	msg.writeData(authPtr1, tunnelSecret)
	msg.writeData(paramsPtr1, tunnelID)
	optData, optPtr0, _ := msg.allocate(1), msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(paramsPtr2, optData, 1, 2)
	ciPtr0, ciPtr1, ciPtr2, ciPtr3 := msg.allocate(1), msg.allocate(1), msg.allocate(1), msg.allocate(1)
	msg.setStructPointer(optPtr0, ciPtr0, 0, 4)
	msg.writeData(ciPtr0, clientID)
	features := []string{"serialized_headers", "ha-connections"}
	msg.writeTextList(ciPtr1, features)
	msg.writeText(ciPtr2, "2024.10.0-Nexus")
	msg.writeText(ciPtr3, "Nexus-Go")
	return msg.toBytes()
}

func startCFTunnel() {
	tokenDataBytes, err := base64.StdEncoding.DecodeString(CFToken)
	if err != nil {
		log.Printf("[ERROR] Invalid CF Tunnel Token: %v", err)
		return
	}

	var tokenData struct {
		A string `json:"a"`
		S string `json:"s"`
		T string `json:"t"`
	}
	if err := json.Unmarshal(tokenDataBytes, &tokenData); err != nil {
		log.Printf("[ERROR] CF Tunnel Token parse error: %v", err)
		return
	}

	tunnelSecret, _ := base64.StdEncoding.DecodeString(tokenData.S)
	tunnelID, _ := uuid.Parse(tokenData.T)

	// Launch 4 connections
	for i := uint8(0); i < 4; i++ {
		go cfTunnelLoop(i, tokenData.A, tunnelSecret, tunnelID[:])
	}
}

type TunnelTransport struct {
	http2.Transport
}

func cfTunnelLoop(connIndex uint8, accountTag string, tunnelSecret, tunnelID []byte) {
	for {
		err := cfTunnelConnect(connIndex, accountTag, tunnelSecret, tunnelID)
		if err != nil {
			log.Printf("[TUNNEL] Conn[%d] closed: %v", connIndex, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func cfTunnelConnect(connIndex uint8, accountTag string, tunnelSecret, tunnelID []byte) error {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		ServerName:         "h2.cftunnel.com",
	}

	edges := []string{"region1.v2.argotunnel.com:7844", "region2.v2.argotunnel.com:7844"}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", edges[rand.Intn(len(edges))], tlsConfig)
	if err != nil {
		return err
	}
	defer conn.Close()

	// In Cloudflare Tunnel protocol, we dial the edge, but act as an HTTP/2 server!
	// The edge sends requests (like control-stream) to us.
	// ReadIdleTimeout 过短会在边缘静默时误杀整条 h2 连接（代理空闲/长视频间隙常见）
	server := &http2.Server{
		ReadIdleTimeout: 5 * time.Minute,
		IdleTimeout:     10 * time.Minute,
	}
	server.ServeConn(conn, &http2.ServeConnOpts{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleEdgeRequest(w, r, connIndex, accountTag, tunnelSecret, tunnelID)
		}),
	})

	return nil
}

func handleEdgeRequest(w http.ResponseWriter, r *http.Request, connIndex uint8, accountTag string, tunnelSecret, tunnelID []byte) {
	// Control Stream: 隧道注册握手
	if r.Header.Get("Cf-Cloudflared-Proxy-Connection-Upgrade") == "control-stream" {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		bsMsg := capnpBootstrap(0)
		w.Write(bsMsg)
		clientID := uuid.New()
		regMsg := capnpRegisterConnection(1, 0, accountTag, tunnelSecret, tunnelID, connIndex, clientID[:])
		w.Write(regMsg)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		log.Printf("[TUNNEL] Conn[%d] registered successfully", connIndex)
		// 保持 control stream 存活
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(15 * time.Second):
				if _, err := w.Write([]byte{}); err != nil {
					return
				}
			}
		}
	}

	webAddr := "127.0.0.1:" + PORT
	upgradeHint := strings.ToLower(r.Header.Get("Cf-Cloudflared-Proxy-Connection-Upgrade"))
	isWebSocket := upgradeHint == "websocket" ||
		r.Header.Get("Sec-Websocket-Key") != "" ||
		r.Header.Get("Sec-WebSocket-Key") != "" ||
		strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	isGRPC := strings.HasPrefix(ct, "application/grpc") ||
		strings.Contains(strings.ToLower(r.URL.Path), "/"+strings.ToLower(GRPCServiceName)+"/")

	if isWebSocket {
		// ---- WebSocket 代理 → Web 端口（再桥到 sing-box WS）----
		localConn, err := net.Dial("tcp", webAddr)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer localConn.Close()

		// 重建 HTTP/1.1 升级请求
		var reqBuf strings.Builder
		reqBuf.WriteString(fmt.Sprintf("GET %s HTTP/1.1\r\n", r.URL.RequestURI()))
		hasHost := false
		hasWSKey := false
		hasWSVersion := false
		for k, vv := range r.Header {
			kLower := strings.ToLower(k)
			if kLower == "connection" || kLower == "upgrade" {
				continue
			}
			if kLower == "host" {
				hasHost = true
			}
			if kLower == "sec-websocket-key" {
				hasWSKey = true
			}
			if kLower == "sec-websocket-version" {
				hasWSVersion = true
			}
			for _, v := range vv {
				reqBuf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
			}
		}
		if !hasHost {
			reqBuf.WriteString(fmt.Sprintf("Host: %s\r\n", r.Host))
		}
		if !hasWSKey {
			reqBuf.WriteString(fmt.Sprintf("Sec-WebSocket-Key: %s\r\n", newWebSocketKey()))
		}
		if !hasWSVersion {
			reqBuf.WriteString("Sec-WebSocket-Version: 13\r\n")
		}
		reqBuf.WriteString("Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		localConn.Write([]byte(reqBuf.String()))

		// 读取本地 HTTP/1.1 响应头
		br := bufio.NewReader(localConn)
		resp, err := http.ReadResponse(br, r)
		if err != nil {
			return
		}

		// 转发响应头给 CF 边缘（101 -> 200）
		for k, vv := range resp.Header {
			kLower := strings.ToLower(k)
			if kLower == "connection" || kLower == "upgrade" || kLower == "transfer-encoding" || kLower == "keep-alive" {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		status := resp.StatusCode
		if status == http.StatusSwitchingProtocols {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// 双向数据桥接
		go func() {
			io.Copy(localConn, r.Body)
			if tcpConn, ok := localConn.(*net.TCPConn); ok {
				tcpConn.CloseWrite()
			}
		}()

		done := make(chan struct{})
		go func() {
			io.Copy(flushWriter{w: w}, br)
			close(done)
		}()

		select {
		case <-done:
		case <-r.Context().Done():
		}
		return
	}

	if isGRPC {
		// ---- gRPC 流式代理：不 ReadAll，对本机 h2c 转到 VLESS-gRPC 端口 ----
		if singBoxGRPCListenPort == 0 {
			log.Printf("[TUNNEL] gRPC request but GRPC_PORT not enabled path=%s", r.URL.RequestURI())
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		log.Printf("[TUNNEL] gRPC stream via CF → 127.0.0.1:%d path=%s", singBoxGRPCListenPort, r.URL.RequestURI())
		if err := proxyGRPCToOrigin(w, r); err != nil {
			log.Printf("[TUNNEL] gRPC proxy error path=%s: %v", r.URL.RequestURI(), err)
		}
		return
	}

	// ---- 普通 HTTP 代理（主页、订阅等）→ Web 端口 ----
	bodyData, _ := io.ReadAll(r.Body)
	targetURL := fmt.Sprintf("http://%s%s", webAddr, r.URL.RequestURI())
	proxyReq, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(bodyData))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	for k, vv := range r.Header {
		kLower := strings.ToLower(k)
		if kLower == "host" {
			continue
		}
		for _, v := range vv {
			proxyReq.Header.Add(k, v)
		}
	}
	proxyReq.Host = r.Host

	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// hop-by-hop 头，反代时必须剥掉（RFC 7230）
var grpcHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-connection":    true,
}

// 全局 h2c Transport：多路复用到本机 gRPC。
// 参考 cloudflared：gRPC 需要及时 flush，但用 32KiB 块而不是每字节一帧。
var grpcH2cTransport = &http2.Transport{
	AllowHTTP: true,
	// AllowHTTP 时可用 http:// URL；DialTLSContext 仍用于建立底层连接
	DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	},
	ReadIdleTimeout: 30 * time.Second,
	PingTimeout:     10 * time.Second,
	// 略增并发 stream，测速会开很多子连接
	MaxHeaderListSize: 262144,
}

// proxyGRPCToOrigin 将 CF 边缘的 gRPC 流以 h2c 转到本机 VLESS-gRPC。
// 不用 ReverseProxy 默认行为：手动 RoundTrip + 分块 flush，便于控制帧大小与 trailer。
func proxyGRPCToOrigin(w http.ResponseWriter, r *http.Request) error {
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", singBoxGRPCListenPort)

	outReq := r.Clone(r.Context())
	outReq.URL = &url.URL{
		Scheme:   "http", // AllowHTTP=true 的 h2c；勿用 https 以免部分路径走错
		Host:     grpcAddr,
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}
	outReq.RequestURI = ""
	outReq.Host = grpcAddr
	// 流式 body：不要预设 ContentLength 缓冲整包
	if outReq.Body == nil {
		outReq.Body = http.NoBody
	}

	for h := range grpcHopHeaders {
		outReq.Header.Del(h)
	}
	// 删除可能由 CF 注入、干扰本机 h2c 的头
	outReq.Header.Del("Cf-Cloudflared-Proxy-Connection-Upgrade")

	resp, err := grpcH2cTransport.RoundTrip(outReq)
	if err != nil {
		grpcH2cTransport.CloseIdleConnections()
		w.WriteHeader(http.StatusBadGateway)
		return fmt.Errorf("h2c RoundTrip %s: %w", grpcAddr, err)
	}
	defer resp.Body.Close()

	for h := range grpcHopHeaders {
		resp.Header.Del(h)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// 预先声明 trailer，否则写完 body 后再加无效
	if len(resp.Trailer) > 0 {
		for k := range resp.Trailer {
			w.Header().Add("Trailer", k)
		}
	}

	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 32KiB 分块读+flush：兼顾 gRPC 流式及时性与测速时不要每字节一帧
	buf := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			if ew != nil {
				grpcH2cTransport.CloseIdleConnections()
				return fmt.Errorf("write to edge: %w", ew)
			}
			if nw != nr {
				return io.ErrShortWrite
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if er != nil {
			if er != io.EOF && r.Context().Err() == nil {
				grpcH2cTransport.CloseIdleConnections()
				return fmt.Errorf("read origin: %w", er)
			}
			break
		}
	}

	for k, vv := range resp.Trailer {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	return nil
}

func newWebSocketKey() string {
	var key [16]byte
	if _, err := crand.Read(key[:]); err != nil {
		return base64.StdEncoding.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano))[:16])
	}
	return base64.StdEncoding.EncodeToString(key[:])
}

type flushWriter struct {
	w http.ResponseWriter
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if f, ok := fw.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}
