package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var tlsPorts = map[string]bool{
	"443": true, "8443": true, "2096": true, "2087": true, "2083": true, "2053": true,
}

var blockedDomains = []string{
	"speedtest.net", "fast.com", "speedtest.cn", "speed.cloudflare.com", "speedof.me",
	"testmy.net", "bandwidth.place", "speed.io", "librespeed.org", "speedcheck.org",
}

func envBool(name string, fallback bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func stripScheme(value string) string {
	text := strings.TrimSpace(value)
	if idx := strings.Index(text, "://"); idx >= 0 {
		text = text[idx+3:]
	}
	return strings.Trim(text, "/")
}

func extractPort(value string) string {
	text := stripScheme(value)
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "[") {
		closing := strings.Index(text, "]")
		if closing >= 0 && closing+1 < len(text) && text[closing+1] == ':' {
			return text[closing+2:]
		}
		return ""
	}
	first := strings.Index(text, ":")
	last := strings.LastIndex(text, ":")
	if first >= 0 && first == last && last < len(text)-1 {
		return text[last+1:]
	}
	return ""
}

func hasExplicitPort(value string) bool {
	return extractPort(value) != ""
}

func resolveNezhaTarget(server, port string) string {
	host := stripScheme(server)
	if host == "" {
		return ""
	}
	if hasExplicitPort(host) {
		return host
	}
	resolvedPort := strings.TrimSpace(port)
	if resolvedPort == "" {
		return host
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + resolvedPort
}

func splitHostPort(value string) (string, string, error) {
	text := strings.TrimSpace(value)
	if strings.HasPrefix(text, "[") {
		host, port, err := net.SplitHostPort(text)
		return host, port, err
	}
	if strings.Count(text, ":") != 1 {
		return "", "", fmt.Errorf("invalid host:port: %s", value)
	}
	parts := strings.SplitN(text, ":", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid host:port: %s", value)
	}
	return parts[0], parts[1], nil
}

func formatHostPort(host, port string) string {
	return net.JoinHostPort(host, port)
}

func parseDoHEndpoints(value string) []string {
	parts := strings.Split(value, ",")
	endpoints := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			endpoints = append(endpoints, trimmed)
		}
	}
	return endpoints
}

func isIPAddress(value string) bool {
	return net.ParseIP(value) != nil
}

func isBlockedDomain(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, blocked := range blockedDomains {
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return true
		}
	}
	return false
}

func resolveWithDoH(ctx context.Context, host string, endpoints []string) string {
	if host == "" || isIPAddress(host) || len(endpoints) == 0 {
		return host
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, recordType := range []string{"A", "AAAA"} {
		for _, endpoint := range endpoints {
			req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
			if err != nil {
				continue
			}
			req.Header.Set("Accept", "application/dns-json")
			req.Header.Set("User-Agent", "OneImg-Go/1.0")
			query := req.URL.Query()
			query.Set("name", host)
			query.Set("type", recordType)
			req.URL.RawQuery = query.Encode()
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			var payload struct {
				Status int `json:"Status"`
				Answer []struct {
					Type int    `json:"type"`
					Data string `json:"data"`
				} `json:"Answer"`
			}
			err = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || payload.Status != 0 {
				continue
			}
			expectedType := 1
			if recordType == "AAAA" {
				expectedType = 28
			}
			for _, answer := range payload.Answer {
				if answer.Type == expectedType && answer.Data != "" {
					return answer.Data
				}
			}
		}
	}
	return host
}

func resolveProxyHost(ctx context.Context, host string) string {
	if host == "" || isIPAddress(host) {
		return host
	}
	return resolveWithDoH(ctx, host, []string{"https://dns.google/resolve"})
}

func isPortAvailable(port string) bool {
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func findAvailablePort(start int, maxAttempts int) string {
	for port := start; port < start+maxAttempts; port++ {
		candidate := strconv.Itoa(port)
		if isPortAvailable(candidate) {
			return candidate
		}
	}
	return ""
}
