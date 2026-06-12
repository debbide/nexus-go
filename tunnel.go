package main

import (
	"bufio"
	"bytes"
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
	msg.writeText(ciPtr2, "2024.10.0-OneImg")
	msg.writeText(ciPtr3, "OneImg-Go")
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
	server := &http2.Server{
		ReadIdleTimeout: 30 * time.Second,
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
			time.Sleep(15 * time.Second)
			if _, err := w.Write([]byte{}); err != nil {
				break
			}
		}
		return
	}

	localAddr := "127.0.0.1:" + PORT
	upgradeHint := strings.ToLower(r.Header.Get("Cf-Cloudflared-Proxy-Connection-Upgrade"))
	isWebSocket := upgradeHint == "websocket" ||
		r.Header.Get("Sec-Websocket-Key") != "" ||
		r.Header.Get("Sec-WebSocket-Key") != "" ||
		strings.ToLower(r.Header.Get("Upgrade")) == "websocket"

	if isWebSocket {
		// ---- WebSocket 代理 ----
		localConn, err := net.Dial("tcp", localAddr)
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
		go io.Copy(localConn, r.Body)
		io.Copy(flushWriter{w: w}, br)

	} else {
		// ---- 普通 HTTP 代理（主页、订阅等）----
		// 读取请求体
		bodyData, _ := io.ReadAll(r.Body)

		// 构建转发请求
		targetURL := fmt.Sprintf("http://%s%s", localAddr, r.URL.RequestURI())
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

		// 转发响应头
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// 转发响应体
		io.Copy(w, resp.Body)
	}
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
