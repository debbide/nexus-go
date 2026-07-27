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
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"
)

// tunnelLog always writes tunnel diagnostics to stderr (captured in app.log).
var tunnelLog = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile)

// gRPC 诊断：并发流计数 + 单调 id，方便对照「测速挂」时间点
var (
	grpcStreamSeq  atomic.Uint64
	grpcStreamLive atomic.Int64
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
		tunnelLog.Printf("[ERROR] Invalid CF Tunnel Token: %v", err)
		return
	}

	var tokenData struct {
		A string `json:"a"`
		S string `json:"s"`
		T string `json:"t"`
	}
	if err := json.Unmarshal(tokenDataBytes, &tokenData); err != nil {
		tunnelLog.Printf("[ERROR] CF Tunnel Token parse error: %v", err)
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
			tunnelLog.Printf("[TUNNEL] Conn[%d] closed: %v", connIndex, err)
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
	// ReadIdleTimeout 过短会在边缘静默时误杀整条 h2 连接（代理空�?长视频间隙常见）
	server := &http2.Server{
		ReadIdleTimeout:       5 * time.Minute,
		IdleTimeout:           10 * time.Minute,
		MaxConcurrentStreams:   32,
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
		tunnelLog.Printf("[TUNNEL] Conn[%d] registered successfully", connIndex)
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
	ctRaw := r.Header.Get("Content-Type")
	ct := strings.ToLower(ctRaw)
	svcName := strings.ToLower(GRPCServiceName)
	if svcName == "" {
		svcName = "gunservice"
	}
	pathLower := strings.ToLower(r.URL.Path)
	isGRPC := strings.HasPrefix(ct, "application/grpc") ||
		strings.Contains(pathLower, "/"+svcName+"/")

	// 总入口：任何�?control 请求都先打一行，失败也能看见「有没有进来、被判成啥�?
	branch := "http"
	if isWebSocket {
		branch = "ws"
	} else if isGRPC {
		branch = "grpc"
	}
	tunnelLog.Printf("[TUNNEL] IN conn=%d branch=%s method=%s path=%s ct=%q upgrade=%q cf-upgrade=%q proto=%s host=%s",
		connIndex, branch, r.Method, r.URL.RequestURI(), ctRaw,
		r.Header.Get("Upgrade"), r.Header.Get("Cf-Cloudflared-Proxy-Connection-Upgrade"),
		r.Proto, r.Host,
	)

	if isWebSocket {
		// ---- WebSocket 代理 �?Web 端口（再桥到 sing-box WS�?---
		wsID := grpcStreamSeq.Add(1) // 复用计数器仅�?id
		wsStart := time.Now()
		tunnelLog.Printf("[TUNNEL] WS#%d start conn=%d path=%s cl=%s",
			wsID, connIndex, r.URL.RequestURI(), r.Header.Get("Content-Length"))
		localConn, err := net.Dial("tcp", webAddr)
		if err != nil {
			tunnelLog.Printf("[TUNNEL] WS#%d dial web failed: %v", wsID, err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer localConn.Close()
		defer func() {
			tunnelLog.Printf("[TUNNEL] WS#%d end dur=%s", wsID, time.Since(wsStart).Round(time.Millisecond))
		}()

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

		// 读取本地 HTTP/1.1 响应�?
		br := bufio.NewReader(localConn)
		resp, err := http.ReadResponse(br, r)
		if err != nil {
			return
		}

		// 转发响应头给 CF 边缘�?01 -> 200�?
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
			tunnelLog.Printf("[TUNNEL] gRPC drop: GRPC_PORT off path=%s ct=%q method=%s",
				r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Method)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		id := grpcStreamSeq.Add(1)
		live := grpcStreamLive.Add(1)
		start := time.Now()
		tunnelLog.Printf("[TUNNEL] gRPC#%d START live=%d conn=%d -> 127.0.0.1:%d method=%s path=%s ct=%q TE=%q cl=%s proto=%s",
				id, live, connIndex, singBoxGRPCListenPort,
				r.Method, r.URL.RequestURI(),
				r.Header.Get("Content-Type"),
				r.Header.Get("TE"),
				r.Header.Get("Content-Length"),
				r.Proto,
			)
		err := proxyGRPCToOrigin(w, r)
		live = grpcStreamLive.Add(-1)
		dur := time.Since(start).Round(time.Millisecond)
		if err != nil {
			tunnelLog.Printf("[TUNNEL] gRPC#%d FAIL live=%d dur=%s path=%s err=%v",
				id, live, dur, r.URL.RequestURI(), err)
		} else {
			tunnelLog.Printf("[TUNNEL] gRPC#%d OK live=%d dur=%s path=%s",
				id, live, dur, r.URL.RequestURI())
		}
		return
	}

	// �?WS/�?gRPC：打一条采样日志，避免完全盲（�?DEBUG 时已全局打开则每条都记可能刷屏，这里只记可疑长请求）
	if r.Method == http.MethodPost || r.Header.Get("Content-Type") != "" {
		tunnelLog.Printf("[TUNNEL] HTTP other conn=%d method=%s path=%s ct=%q cl=%s",
			connIndex, r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get("Content-Length"))
	}

	// ---- 普�?HTTP 代理（主页、订阅等）→ Web 端口 ----
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

// hop-by-hop: do NOT strip "te" — gRPC needs TE: trailers end-to-end.
var grpcHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-connection":    true,
}

// Shared h2c transport to local VLESS-gRPC (grpclite).
var grpcH2cTransport = &http2.Transport{
	AllowHTTP: true,
	DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	},
	ReadIdleTimeout:   3 * time.Minute,
	PingTimeout:       15 * time.Second,
	MaxHeaderListSize: 262144,
}

// isConnError returns true only for transport-level failures that warrant
// clearing the h2c connection pool. Stream-level errors (context cancel,
// EOF on a single stream) must NOT trigger CloseIdleConnections, or every
// concurrent speed-test stream will be torn down at once.
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "dial tcp")
}

// proxyGRPCToOrigin uses std ReverseProxy for full-duplex body + trailers.
// Keep TE: trailers; strip CF inject headers; flush every 10ms (not every byte).
func proxyGRPCToOrigin(w http.ResponseWriter, r *http.Request) error {
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", singBoxGRPCListenPort)
	target, err := url.Parse("http://" + grpcAddr)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return err
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = grpcH2cTransport
	// 10ms batching: stream promptly without 1-byte HTTP/2 frames to CF edge.
	proxy.FlushInterval = 10 * time.Millisecond
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, e error) {
		if isConnError(e) {
			grpcH2cTransport.CloseIdleConnections()
		}
		tunnelLog.Printf("[TUNNEL] gRPC ReverseProxy error path=%s: %v", req.URL.RequestURI(), e)
		rw.WriteHeader(http.StatusBadGateway)
	}
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = grpcAddr
		for h := range grpcHopHeaders {
			req.Header.Del(h)
		}
		// Drop Cloudflare injects that confuse origin.
		for k := range req.Header {
			if strings.HasPrefix(strings.ToLower(k), "cf-") {
				req.Header.Del(k)
			}
		}
		req.Header.Set("TE", "trailers")
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/grpc")
		}
		// Streaming body.
		req.ContentLength = -1
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		for h := range grpcHopHeaders {
			resp.Header.Del(h)
		}
		// Ensure trailer declaration for gRPC status.
		if resp.Header.Get("Trailer") == "" {
			resp.Header.Add("Trailer", "Grpc-Status")
			resp.Header.Add("Trailer", "Grpc-Message")
		}
		tunnelLog.Printf("[TUNNEL] gRPC origin headers status=%d ct=%q",
			resp.StatusCode, resp.Header.Get("Content-Type"))
		return nil
	}

	// Capture cancel as failure for logs (ServeHTTP itself doesn't return error).
	proxy.ServeHTTP(w, r)
	if err := r.Context().Err(); err != nil {
		return fmt.Errorf("edge canceled: %w", err)
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
