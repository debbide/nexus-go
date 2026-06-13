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
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing/common/json/badoption"
)

const (
	singBoxTUICDefaultName = "nexus"
)

// singBoxVLESSListenPort 在启动时动态分配，避免多实例端口冲突
var singBoxVLESSListenPort uint16

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

	tuicPort, err := parseUint16Port(TUICPort)
	if err != nil {
		return nil, fmt.Errorf("invalid TUIC_PORT: %w", err)
	}

	certPEM, keyPEM, err := generateSelfSignedCertificate(resolveTUICServerName())
	if err != nil {
		return nil, fmt.Errorf("generate TUIC certificate: %w", err)
	}

	listenLocal := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	listenAll := badoption.Addr(netip.IPv4Unspecified())
	ctx := minimalSingBoxContext(context.Background())

	instance, err := box.New(box.Options{
		Context: ctx,
		Options: option.Options{
			Log: &option.LogOptions{
				Disabled: !Debug,
				Level:    singBoxLogLevel(),
			},
			Inbounds: []option.Inbound{
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
				{
					Type: constant.TypeTUIC,
					Tag:  "tuic-in",
					Options: &option.TUICInboundOptions{
						ListenOptions: option.ListenOptions{
							Listen:     &listenAll,
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
				},
			},
			Outbounds: []option.Outbound{
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
			},
			Route: &option.RouteOptions{Final: "direct"},
		},
	})
	if err != nil {
		return nil, err
	}
	if err := instance.Start(); err != nil {
		instance.Close()
		return nil, err
	}
	log.Printf("[INFO] sing-box runtime started: vless-ws=127.0.0.1:%d%s tuic=0.0.0.0:%s", singBoxVLESSListenPort, singBoxVLESSPath(), TUICPort)
	return &singBoxRuntime{instance: instance}, nil
}

func minimalSingBoxContext(ctx context.Context) context.Context {
	inboundRegistry := inbound.NewRegistry()
	vless.RegisterInbound(inboundRegistry)
	tuic.RegisterInbound(inboundRegistry)

	outboundRegistry := outbound.NewRegistry()
	direct.RegisterOutbound(outboundRegistry)

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
