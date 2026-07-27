package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/constant"
	boxDNS "github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing/common/json/badoption"
)

const (
	singBoxTUICDefaultName = "nexus"
)

// singBoxVLESSListenPort 在启动时动态分配，避免多实例端口冲突
var singBoxVLESSListenPort uint16

// singBoxGRPCListenPort 本机 VLESS-gRPC（h2c）端口；0 表示未启用
var singBoxGRPCListenPort uint16

func findFreeLocalPort() (uint16, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port), nil
}

type singBoxRuntime struct {
	instance *box.Box
}

func startSingBoxRuntime() (*singBoxRuntime, error) {
	// 动态获取一个空闲的本地端口给 VLESS 内部监听
	vlessPort, err := findFreeLocalPort()
	if err != nil {
		return nil, fmt.Errorf("find free port for VLESS: %w", err)
	}
	singBoxVLESSListenPort = vlessPort


	listenLocal := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	// UDP 入站监听：默认 [::] 双栈；UDP_IPV6_ONLY=true 时绑本机全局 IPv6（真·v6 only）
	listenUDP, listenUDPLabel, listenMode, err := resolveUDPIPv6ListenAddr()
	if err != nil {
		return nil, err
	}
	ctx := minimalSingBoxContext(context.Background())

	inbounds := []option.Inbound{
		{
			Type: constant.TypeVLESS,
			Tag:  "vless-ws-in",
			Options: &option.VLESSInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     &listenLocal,
					ListenPort: singBoxVLESSListenPort,
				},
				Users: []option.VLESSUser{
					{Name: singBoxTUICDefaultName, UUID: UUID},
				},
				Transport: &option.V2RayTransportOptions{
					Type: constant.V2RayTransportTypeWebsocket,
					WebsocketOptions: option.V2RayWebsocketOptions{
						Path: singBoxVLESSPath(),
					},
				},
			},
		},
	}

	// VLESS-gRPC：本机 h2c（无 TLS），供手搓隧道流式反代 / 官方 cloudflared 回源 / IP 直连
	if GRPCPort != "" && GRPCPort != "0" {
		grpcPort, err := parseUint16Port(GRPCPort)
		if err != nil {
			return nil, fmt.Errorf("invalid GRPC_PORT: %w", err)
		}
		// 与 Web PORT 冲突时换空闲端口
		if strconv.Itoa(int(grpcPort)) == PORT || !isPortAvailable(strconv.Itoa(int(grpcPort))) {
			alt, aerr := findFreeLocalPort()
			if aerr != nil {
				return nil, fmt.Errorf("GRPC_PORT %d busy and no free port: %w", grpcPort, aerr)
			}
			log.Printf("[WARN] GRPC_PORT %d unavailable, using %d", grpcPort, alt)
			grpcPort = alt
		}
		singBoxGRPCListenPort = grpcPort
		// 监听 0.0.0.0：直连 IP:端口 + 本机 127.0.0.1 隧道回源均可
		listenAllTCP := badoption.Addr(netip.IPv4Unspecified())
		inbounds = append(inbounds, option.Inbound{
			Type: constant.TypeVLESS,
			Tag:  "vless-grpc-in",
			Options: &option.VLESSInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     &listenAllTCP,
					ListenPort: grpcPort,
				},
				Users: []option.VLESSUser{
					{Name: singBoxTUICDefaultName + "-grpc", UUID: UUID},
				},
				// 无 TLS：本地 h2c。CF 域名 TLS 在边缘终结。
				// ForceLite：使用 grpclite（h2c），与隧道 http2.Transport h2c 对齐
				Transport: &option.V2RayTransportOptions{
					Type: constant.V2RayTransportTypeGRPC,
					GRPCOptions: option.V2RayGRPCOptions{
						ServiceName: GRPCServiceName,
						ForceLite:   true,
					},
				},
			},
		})
	}

	if TUICPort != "" && TUICPort != "0" {
		tuicPort, err := parseUint16Port(TUICPort)
		if err != nil {
			return nil, fmt.Errorf("invalid TUIC_PORT: %w", err)
		}

		certPEM, keyPEM, err := generateSelfSignedCertificate(resolveTUICServerName())
		if err != nil {
			return nil, fmt.Errorf("generate TUIC certificate: %w", err)
		}

		inbounds = append(inbounds, option.Inbound{
			Type: constant.TypeTUIC,
			Tag:  "tuic-in",
			Options: &option.TUICInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     &listenUDP,
					ListenPort: tuicPort,
				},
				Users: []option.TUICUser{
					{Name: singBoxTUICDefaultName, UUID: UUID, Password: TUICPassword},
				},
				CongestionControl: "bbr",
				Heartbeat:         badoption.Duration(10 * time.Second),
				InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
					TLS: &option.InboundTLSOptions{
						Enabled:     true,
						ServerName:  resolveTUICServerName(),
						ALPN:        badoption.Listable[string]{"h3"},
						Certificate: badoption.Listable[string]{string(certPEM)},
						Key:         badoption.Listable[string]{string(keyPEM)},
					},
				},
			},
		})
	}

	if HY2Port != "" && HY2Port != "0" {
		hy2Port, err := parseUint16Port(HY2Port)
		if err != nil {
			return nil, fmt.Errorf("invalid HY2_PORT: %w", err)
		}

		certPEM, keyPEM, err := generateSelfSignedCertificate(resolveHY2ServerName())
		if err != nil {
			return nil, fmt.Errorf("generate HY2 certificate: %w", err)
		}

		hy2Opts := &option.Hysteria2InboundOptions{
			ListenOptions: option.ListenOptions{
				Listen:     &listenUDP,
				ListenPort: hy2Port,
			},
			Users: []option.Hysteria2User{
				{Name: singBoxTUICDefaultName, Password: HY2Password},
			},
			IgnoreClientBandwidth: true,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &option.InboundTLSOptions{
					Enabled:     true,
					ServerName:  resolveHY2ServerName(),
					ALPN:        badoption.Listable[string]{"h3"},
					Certificate: badoption.Listable[string]{string(certPEM)},
					Key:         badoption.Listable[string]{string(keyPEM)},
				},
			},
		}
		if HY2ObfsPass != "" {
			hy2Opts.Obfs = &option.Hysteria2Obfs{
				Type:     "salamander",
				Password: HY2ObfsPass,
			}
		}
		inbounds = append(inbounds, option.Inbound{
			Type:    constant.TypeHysteria2,
			Tag:     "hy2-in",
			Options: hy2Opts,
		})
	}

	outbounds := []option.Outbound{
		{
			Type: constant.TypeDirect,
			Tag:  "direct",
			Options: &option.DirectOutboundOptions{
				DialerOptions: option.DialerOptions{
					ConnectTimeout: badoption.Duration(10 * time.Second),
					TCPKeepAlive:   badoption.Duration(15 * time.Second),
				},
			},
		},
	}
	var routeRules []option.Rule

	// fanout 旁路：每国独立 VLESS-WS 入口 → 对应本机 SOCKS；主入口仍走 direct
	fanoutExits := getFanoutExits()
	for i := range fanoutExits {
		ex := &fanoutExits[i]
		foPort, err := findFreeLocalPort()
		if err != nil {
			return nil, fmt.Errorf("find free port for fanout %s: %w", ex.Code, err)
		}
		ex.ListenPort = foPort
		inbounds = append(inbounds, option.Inbound{
			Type: constant.TypeVLESS,
			Tag:  ex.InboundTag,
			Options: &option.VLESSInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     &listenLocal,
					ListenPort: foPort,
				},
				Users: []option.VLESSUser{
					{Name: singBoxTUICDefaultName + "-fo-" + ex.Code, UUID: UUID},
				},
				Transport: &option.V2RayTransportOptions{
					Type: constant.V2RayTransportTypeWebsocket,
					WebsocketOptions: option.V2RayWebsocketOptions{
						Path: "/" + trimPath(ex.Path),
					},
				},
			},
		})
		outbounds = append(outbounds, option.Outbound{
			Type: constant.TypeSOCKS,
			Tag:  ex.OutboundTag,
			Options: &option.SOCKSOutboundOptions{
				ServerOptions: option.ServerOptions{
					Server:     ex.SocksHost,
					ServerPort: ex.SocksPort,
				},
				Version: "5",
				DialerOptions: option.DialerOptions{
					ConnectTimeout: badoption.Duration(10 * time.Second),
					TCPKeepAlive:   badoption.Duration(15 * time.Second),
				},
			},
		})
		routeRules = append(routeRules, option.Rule{
			Type: constant.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				RawDefaultRule: option.RawDefaultRule{
					Inbound: badoption.Listable[string]{ex.InboundTag},
				},
				RuleAction: option.RuleAction{
					Action: constant.RuleActionTypeRoute,
					RouteOptions: option.RouteActionOptions{
						Outbound: ex.OutboundTag,
					},
				},
			},
		})
	}
	if len(fanoutExits) > 0 {
		setFanoutExits(fanoutExits)
	}

	instance, err := box.New(box.Options{
		Context: ctx,
		Options: option.Options{
			Log: &option.LogOptions{
				Disabled: !Debug,
				Level:    singBoxLogLevel(),
			},
			Inbounds:  inbounds,
			Outbounds: outbounds,
			Route: &option.RouteOptions{
				Rules: routeRules,
				Final: "direct",
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if err := instance.Start(); err != nil {
		instance.Close()
		return nil, err
	}
	parts := []string{fmt.Sprintf("vless-ws=127.0.0.1:%d%s", singBoxVLESSListenPort, singBoxVLESSPath())}
	if singBoxGRPCListenPort > 0 {
		parts = append(parts, fmt.Sprintf("vless-grpc=0.0.0.0:%d(h2c,service=%s)", singBoxGRPCListenPort, GRPCServiceName))
	}
	if TUICPort != "" && TUICPort != "0" {
		parts = append(parts, fmt.Sprintf("tuic=%s:%s(%s)", listenUDPLabel, TUICPort, listenMode))
	}
	if HY2Port != "" && HY2Port != "0" {
		parts = append(parts, fmt.Sprintf("hy2=%s:%s(%s)", listenUDPLabel, HY2Port, listenMode))
	}
	for _, ex := range getFanoutExits() {
		parts = append(parts, fmt.Sprintf("fanout-%s=127.0.0.1:%d/%s->%s:%d", ex.Code, ex.ListenPort, ex.Path, ex.SocksHost, ex.SocksPort))
	}
	log.Printf("[INFO] sing-box runtime started: %s", strings.Join(parts, " "))
	return &singBoxRuntime{instance: instance}, nil
}

// resolveUDPIPv6ListenAddr 返回 TUIC/HY2 的监听地址与日志标签。
// 双栈：:: ；v6 only：本机第一个全局单播 IPv6（避免 [::] 在默认 Linux 上仍接 v4-mapped）。
func resolveUDPIPv6ListenAddr() (badoption.Addr, string, string, error) {
	if !UDPIPv6Only {
		return badoption.Addr(netip.IPv6Unspecified()), "[::]", "dual-stack", nil
	}
	ip, err := firstGlobalIPv6()
	if err != nil {
		return badoption.Addr{}, "", "", fmt.Errorf("UDP_IPV6_ONLY enabled but no global IPv6 found: %w", err)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return badoption.Addr{}, "", "", fmt.Errorf("parse global IPv6 %q: %w", ip, err)
	}
	return badoption.Addr(addr), "[" + ip + "]", "ipv6-only", nil
}

func firstGlobalIPv6() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip = ip.To16()
			if ip == nil || ip.To4() != nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(ip)
			if !ok || !addr.IsGlobalUnicast() {
				continue
			}
			// 排除链路本地等已由 IsGlobalUnicast 处理；再排除常见 ULA 也可保留（ULA 也算 global unicast）
			return addr.String(), nil
		}
	}
	// 回退：公网 API
	if ip := fetchPublicIPv6(); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no suitable IPv6 address on any interface")
}

func minimalSingBoxContext(ctx context.Context) context.Context {
	inboundRegistry := inbound.NewRegistry()
	vless.RegisterInbound(inboundRegistry)
	tuic.RegisterInbound(inboundRegistry)
	hysteria2.RegisterInbound(inboundRegistry)

	outboundRegistry := outbound.NewRegistry()
	direct.RegisterOutbound(outboundRegistry)
	socks.RegisterOutbound(outboundRegistry)

	dnsRegistry := boxDNS.NewTransportRegistry()
	local.RegisterTransport(dnsRegistry)

	return box.Context(
		ctx,
		inboundRegistry,
		outboundRegistry,
		endpoint.NewRegistry(),
		dnsRegistry,
		boxService.NewRegistry(),
	)
}

func (r *singBoxRuntime) Close() {
	if r != nil && r.instance != nil {
		_ = r.instance.Close()
	}
}

func singBoxLogLevel() string {
	if Debug {
		return "info"
	}
	return "error"
}

func singBoxVLESSPath() string {
	return "/" + trimPath(WsPath)
}

func resolveTUICServerName() string {
	if TUICDomain != "" {
		return normalizeTUICHost(TUICDomain)
	}
	if UDPIPv6Only {
		if ip := fetchPublicIPv6(); ip != "" {
			return normalizeTUICHost(ip)
		}
		if ip, err := firstGlobalIPv6(); err == nil {
			return normalizeTUICHost(ip)
		}
	}
	if publicIP := fetchPublicIPv4(); publicIP != "" {
		return normalizeTUICHost(publicIP)
	}
	return "nexus.local"
}

func resolveHY2ServerName() string {
	if HY2Domain != "" {
		return normalizeTUICHost(HY2Domain)
	}
	// 与 TUIC 共用域名配置，方便只填一次
	if TUICDomain != "" {
		return normalizeTUICHost(TUICDomain)
	}
	if UDPIPv6Only {
		if ip := fetchPublicIPv6(); ip != "" {
			return normalizeTUICHost(ip)
		}
		if ip, err := firstGlobalIPv6(); err == nil {
			return normalizeTUICHost(ip)
		}
	}
	if publicIP := fetchPublicIPv4(); publicIP != "" {
		return normalizeTUICHost(publicIP)
	}
	return "nexus.local"
}

func normalizeTUICHost(value string) string {
	host := stripScheme(value)
	if cut := strings.IndexAny(host, "/?#"); cut >= 0 {
		host = host[:cut]
	}
	if strings.HasPrefix(host, "[") {
		if closing := strings.Index(host, "]"); closing >= 0 {
			return host[1:closing]
		}
	}
	if strings.Count(host, ":") == 1 {
		if splitHost, _, err := net.SplitHostPort(host); err == nil && splitHost != "" {
			return splitHost
		}
	}
	return strings.TrimSpace(host)
}

func parseUint16Port(value string) (uint16, error) {
	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port out of range: %d", port)
	}
	return uint16(port), nil
}

func generateSelfSignedCertificate(serverName string) ([]byte, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: serverName,
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(serverName); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{serverName}
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
