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
		tunnelLog.Printf("[TUNNEL] gRPC#%d START live=%d conn=%d �?127.0.0.1:%d method=%s path=%s ct=%q te=%q cl=%s proto=%s",
			id, live, connIndex, singBoxGRPCListenPort,
			r.Method, r.URL.RequestURI(),
			r.Header.Get("Content-Type"),
			r.Header.Get("Transfer-Encoding"),
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

// hop-by-hop 头，反代时必须剥掉（RFC 7230�?
// 不要删除 te：gRPC 需要 TE: trailers（删掉客户端会像完全没反应）
// hop-by-hop 头。不要删 te：gRPC 需要 TE: trailers
var grpcHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-connection":    true,
}

// 全局 h2c Transport：多路复用到本机 gRPC�?
// 参�?cloudflared：gRPC 需要及�?flush，但�?32KiB 块而不是每字节一帧�?
var grpcH2cTransport = &http2.Transport{
	AllowHTTP: true,
	// AllowHTTP 时可�?http:// URL；DialTLSContext 仍用于建立底层连�?
	DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	},
	ReadIdleTimeout: 30 * time.Second,
	PingTimeout:     10 * time.Second,
	// 略增并发 stream，测速会开很多子连�?
	MaxHeaderListSize: 262144,
}

// proxyGRPCToOrigin �?CF 边缘�?gRPC 流以 h2c 转到本机 VLESS-gRPC�?
// 手动 RoundTrip + 32KiB 分块 flush；详细字�?耗时由调用方 gRPC# 日志汇总�?
func proxyGRPCToOrigin(w http.ResponseWriter, r *http.Request) error {
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", singBoxGRPCListenPort)
	t0 := time.Now()

	outReq := r.Clone(r.Context())
	outReq.URL = &url.URL{
		Scheme:   "http", // AllowHTTP=true �?h2c
		Host:     grpcAddr,
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}
	outReq.RequestURI = ""
	outReq.Host = grpcAddr
	if outReq.Body == nil {
		outReq.Body = http.NoBody
	}

	for h := range grpcHopHeaders {
		outReq.Header.Del(h)
	}
	outReq.Header.Del("Cf-Cloudflared-Proxy-Connection-Upgrade")
	// gRPC 强制需要；CF 或 hop 清理后可能丢失
	outReq.Header.Set("TE", "trailers")
	if outReq.Header.Get("Content-Type") == "" {
		outReq.Header.Set("Content-Type", "application/grpc")
	}

	resp, err := grpcH2cTransport.RoundTrip(outReq)
	headersDur := time.Since(t0).Round(time.Millisecond)
	if err != nil {
		grpcH2cTransport.CloseIdleConnections()
		w.WriteHeader(http.StatusBadGateway)
		return fmt.Errorf("h2c RoundTrip %s after %s: %w", grpcAddr, headersDur, err)
	}
	defer resp.Body.Close()

	tunnelLog.Printf("[TUNNEL] gRPC origin headers status=%d dur=%s ct=%q",
		resp.StatusCode, headersDur, resp.Header.Get("Content-Type"))

	for h := range grpcHopHeaders {
		resp.Header.Del(h)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if len(resp.Trailer) > 0 {
		for k := range resp.Trailer {
			w.Header().Add("Trailer", k)
		}
	}

	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	var written int64
	buf := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				grpcH2cTransport.CloseIdleConnections()
				return fmt.Errorf("write to edge after %d bytes: %w", written, ew)
			}
			if nw != nr {
				return fmt.Errorf("short write to edge after %d bytes", written)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if er != nil {
			if er == io.EOF {
				break
			}
			if r.Context().Err() != nil {
				return fmt.Errorf("edge canceled after %d bytes: %w (ctx=%v)", written, er, r.Context().Err())
			}
			grpcH2cTransport.CloseIdleConnections()
			return fmt.Errorf("read origin after %d bytes: %w", written, er)
		}
	}

	for k, vv := range resp.Trailer {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	tunnelLog.Printf("[TUNNEL] gRPC body done bytes=%d bodyDur=%s totalDur=%s",
		written,
		time.Since(t0).Round(time.Millisecond)-headersDur,
		time.Since(t0).Round(time.Millisecond),
	)
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
