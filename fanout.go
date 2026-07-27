package main

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
)

// FanoutExit 描述一个 fanout 旁路出口：一国（或一槽）→ 本机 SOCKS → 独立 VLESS 入口 path。
// 主代理（vless-ws-in / tuic / hy2）不走这里。
type FanoutExit struct {
	Code        string // 短码，如 jp / us，用于 path 与订阅备注
	SocksHost   string
	SocksPort   uint16
	Path        string // WS path，不含前导 /，如 fo-jp
	InboundTag  string
	OutboundTag string
	ListenPort  uint16 // sing-box 本机 VLESS 监听端口，启动时分配
}

var (
	fanoutMu    sync.RWMutex
	FanoutExits []FanoutExit
)

// parseFanoutExits 解析 FANOUT_EXITS。
// 格式：jp:127.0.0.1:11080,us:11081,kr:127.0.0.1:11082
// 仅端口时默认 host=127.0.0.1。
func parseFanoutExits(raw, pathPrefix string) ([]FanoutExit, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	pathPrefix = trimPath(pathPrefix)
	if pathPrefix == "" {
		pathPrefix = "fo"
	}

	parts := strings.Split(raw, ",")
	exits := make([]FanoutExit, 0, len(parts))
	seenCode := map[string]struct{}{}
	seenPath := map[string]struct{}{}

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		code, host, port, err := splitFanoutEntry(part)
		if err != nil {
			return nil, err
		}
		codeLower := strings.ToLower(code)
		if _, ok := seenCode[codeLower]; ok {
			return nil, fmt.Errorf("duplicate fanout code: %s", code)
		}
		path := pathPrefix + "-" + sanitizeFanoutCode(codeLower)
		if path == trimPath(WsPath) {
			return nil, fmt.Errorf("fanout path %q collides with main WSPATH", path)
		}
		if _, ok := seenPath[path]; ok {
			return nil, fmt.Errorf("duplicate fanout path: %s", path)
		}
		seenCode[codeLower] = struct{}{}
		seenPath[path] = struct{}{}

		tagSuffix := sanitizeFanoutCode(codeLower)
		exits = append(exits, FanoutExit{
			Code:        codeLower,
			SocksHost:   host,
			SocksPort:   port,
			Path:        path,
			InboundTag:  "vless-fanout-" + tagSuffix,
			OutboundTag: "socks-fanout-" + tagSuffix,
		})
	}
	return exits, nil
}

func splitFanoutEntry(part string) (code, host string, port uint16, err error) {
	// code:host:port 或 code:port
	// 先按第一个 ':' 切开 code
	idx := strings.IndexByte(part, ':')
	if idx <= 0 || idx == len(part)-1 {
		return "", "", 0, fmt.Errorf("invalid FANOUT_EXITS entry %q (want code:port or code:host:port)", part)
	}
	code = strings.TrimSpace(part[:idx])
	rest := strings.TrimSpace(part[idx+1:])
	if code == "" || rest == "" {
		return "", "", 0, fmt.Errorf("invalid FANOUT_EXITS entry %q", part)
	}
	if err := validateFanoutCode(code); err != nil {
		return "", "", 0, err
	}

	// rest 是 port 或 host:port（host 可能是 IPv4）
	if !strings.Contains(rest, ":") {
		p, e := strconv.Atoi(rest)
		if e != nil || p < 1 || p > 65535 {
			return "", "", 0, fmt.Errorf("invalid fanout port in %q", part)
		}
		return code, "127.0.0.1", uint16(p), nil
	}

	h, pStr, e := net.SplitHostPort(rest)
	if e != nil {
		// 尝试 host:port 手工拆（IPv4）
		last := strings.LastIndexByte(rest, ':')
		h = rest[:last]
		pStr = rest[last+1:]
	}
	h = strings.TrimSpace(h)
	pStr = strings.TrimSpace(pStr)
	if h == "" {
		h = "127.0.0.1"
	}
	p, e := strconv.Atoi(pStr)
	if e != nil || p < 1 || p > 65535 {
		return "", "", 0, fmt.Errorf("invalid fanout port in %q", part)
	}
	return code, h, uint16(p), nil
}

func validateFanoutCode(code string) error {
	if code == "" {
		return fmt.Errorf("empty fanout code")
	}
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("invalid fanout code %q (use letters/digits/-/_)", code)
	}
	return nil
}

func sanitizeFanoutCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" {
		return "x"
	}
	return s
}

func setFanoutExits(exits []FanoutExit) {
	fanoutMu.Lock()
	 defer fanoutMu.Unlock()
	FanoutExits = exits
}

func getFanoutExits() []FanoutExit {
	fanoutMu.RLock()
	defer fanoutMu.RUnlock()
	out := make([]FanoutExit, len(FanoutExits))
	copy(out, FanoutExits)
	return out
}

func findFanoutByPath(reqPath string) (FanoutExit, bool) {
	reqPath = trimPath(reqPath)
	for _, ex := range getFanoutExits() {
		if trimPath(ex.Path) == reqPath {
			return ex, true
		}
	}
	return FanoutExit{}, false
}

func logFanoutSummary() {
	exits := getFanoutExits()
	if len(exits) == 0 {
		return
	}
	parts := make([]string, 0, len(exits))
	for _, ex := range exits {
		parts = append(parts, fmt.Sprintf("%s=/%s->%s:%d", ex.Code, ex.Path, ex.SocksHost, ex.SocksPort))
	}
	log.Printf("[INFO] fanout bypass enabled: %s", strings.Join(parts, " "))
}
