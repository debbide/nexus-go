package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func dispatchProxyProtocol(conn *websocket.Conn, firstMsg []byte) {
	defer conn.Close()
	if len(firstMsg) == 0 {
		return
	}

	if firstMsg[0] == 0 && len(firstMsg) > 17 {
		if handleVLESS(conn, firstMsg) {
			return
		}
	}

	if len(firstMsg) >= 58 {
		if handleTrojan(conn, firstMsg) {
			return
		}
	}

	atyp := firstMsg[0]
	if atyp == 1 || atyp == 3 || atyp == 4 {
		handleShadowsocks(conn, firstMsg)
	}
}

func parseAddressValue(data []byte, offset int, atyp byte, domainType byte, ipv6Type byte) (string, int, error) {
	switch atyp {
	case 1:
		if offset+4 > len(data) {
			return "", 0, fmt.Errorf("ipv4 address is incomplete")
		}
		return net.IP(data[offset : offset+4]).String(), offset + 4, nil
	case domainType:
		if offset >= len(data) {
			return "", 0, fmt.Errorf("domain length is missing")
		}
		domainLen := int(data[offset])
		offset++
		if offset+domainLen > len(data) {
			return "", 0, fmt.Errorf("domain address is incomplete")
		}
		return string(data[offset : offset+domainLen]), offset + domainLen, nil
	case ipv6Type:
		if offset+16 > len(data) {
			return "", 0, fmt.Errorf("ipv6 address is incomplete")
		}
		return net.IP(data[offset : offset+16]).String(), offset + 16, nil
	default:
		return "", 0, fmt.Errorf("unsupported address type: %d", atyp)
	}
}

func parseVLESSTarget(data []byte, offset int) (string, uint16, int, error) {
	if offset+3 > len(data) {
		return "", 0, 0, fmt.Errorf("vless target is incomplete")
	}
	port := binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2
	atyp := data[offset]
	offset++
	host, next, err := parseAddressValue(data, offset, atyp, 2, 3)
	return host, port, next, err
}

func parseSocksTarget(data []byte, offset int) (string, uint16, int, error) {
	if offset >= len(data) {
		return "", 0, 0, fmt.Errorf("socks target address type is missing")
	}
	atyp := data[offset]
	offset++
	host, offset, err := parseAddressValue(data, offset, atyp, 3, 4)
	if err != nil {
		return "", 0, 0, err
	}
	if offset+2 > len(data) {
		return "", 0, 0, fmt.Errorf("socks target port is incomplete")
	}
	port := binary.BigEndian.Uint16(data[offset : offset+2])
	return host, port, offset + 2, nil
}

func dialAddress(host string, port uint16) (string, bool) {
	if isBlockedDomain(host) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resolvedHost := resolveProxyHost(ctx, host)
	if resolvedHost == "" {
		resolvedHost = host
	}
	return net.JoinHostPort(resolvedHost, strconv.Itoa(int(port))), true
}

func handleVLESS(conn *websocket.Conn, firstMsg []byte) bool {
	if len(firstMsg) < 18 || firstMsg[0] != 0 {
		return false
	}

	expectedUUID, err := hex.DecodeString(strings.ReplaceAll(UUID, "-", ""))
	if err != nil || !bytes.Equal(firstMsg[1:17], expectedUUID) {
		return false
	}

	offset := 18 + int(firstMsg[17])
	if offset >= len(firstMsg) {
		return false
	}
	cmd := firstMsg[offset]
	offset++
	if cmd != 1 {
		return false
	}

	host, port, nextOffset, err := parseVLESSTarget(firstMsg, offset)
	if err != nil {
		return false
	}
	addr, ok := dialAddress(host, port)
	if !ok {
		return false
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0, 0}); err != nil {
		return true
	}
	bridge(conn, addr, firstMsg[nextOffset:])
	return true
}

func handleTrojan(conn *websocket.Conn, firstMsg []byte) bool {
	if len(firstMsg) < 58 {
		return false
	}

	receivedHex := string(firstMsg[:56])

	hashNoDash := sha256.New224()
	hashNoDash.Write([]byte(strings.ReplaceAll(UUID, "-", "")))
	expectedNoDash := hex.EncodeToString(hashNoDash.Sum(nil))

	hashDashed := sha256.New224()
	hashDashed.Write([]byte(UUID))
	expectedDashed := hex.EncodeToString(hashDashed.Sum(nil))

	if receivedHex != expectedNoDash && receivedHex != expectedDashed {
		return false
	}

	offset := 56
	if offset+2 <= len(firstMsg) && firstMsg[offset] == '\r' && firstMsg[offset+1] == '\n' {
		offset += 2
	}
	if offset >= len(firstMsg) {
		return false
	}

	cmd := firstMsg[offset]
	offset++
	if cmd != 1 {
		return false
	}

	host, port, nextOffset, err := parseSocksTarget(firstMsg, offset)
	if err != nil {
		return false
	}
	if nextOffset+2 <= len(firstMsg) && firstMsg[nextOffset] == '\r' && firstMsg[nextOffset+1] == '\n' {
		nextOffset += 2
	}

	addr, ok := dialAddress(host, port)
	if !ok {
		return false
	}
	bridge(conn, addr, firstMsg[nextOffset:])
	return true
}

func handleShadowsocks(conn *websocket.Conn, firstMsg []byte) bool {
	host, port, nextOffset, err := parseSocksTarget(firstMsg, 0)
	if err != nil {
		return false
	}
	addr, ok := dialAddress(host, port)
	if !ok {
		return false
	}
	bridge(conn, addr, firstMsg[nextOffset:])
	return true
}

func bridge(conn *websocket.Conn, addr string, initialData []byte) {
	tcpConn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("[PROXY] Failed to connect to %s: %v", addr, err)
		return
	}
	defer tcpConn.Close()

	if len(initialData) > 0 {
		if _, err := tcpConn.Write(initialData); err != nil {
			return
		}
	}

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if _, err := tcpConn.Write(msg); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				if err2 := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err2 != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[PROXY] TCP read error from %s: %v", addr, err)
				}
				return
			}
		}
	}()

	<-done
}
