package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/nezhahq/agent/proto"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

const nezhaAgentVersion = "OneImg-Go v1.0.0"

var (
	lastNetIn         uint64
	lastNetOut        uint64
	lastNetTime       int64
	bootTime          uint64
	dashboardBootTime uint64
)

func init() {
	bootTime = uint64(time.Now().Unix())
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "btime ") {
				var parsed uint64
				fmt.Sscanf(strings.TrimPrefix(line, "btime "), "%d", &parsed)
				if parsed > 0 {
					bootTime = parsed
				}
				break
			}
		}
	}
}

func startNezhaAgent() {
	if NezhaServer == "" || NezhaKey == "" {
		log.Printf("[NEZHA] Skipped (NEZHA_SERVER or NEZHA_KEY not set)")
		return
	}
	target := resolveNezhaTarget(NezhaServer, NezhaPort)
	if target == "" || !hasExplicitPort(target) {
		log.Printf("[NEZHA] NEZHA_SERVER must include a port, or NEZHA_PORT must be set")
		return
	}
	log.Printf("[NEZHA] Starting Agent, connecting to %s", target)
	for {
		if err := nezhaLoop(target); err != nil {
			log.Printf("[NEZHA] Disconnected: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func nezhaLoop(target string) error {
	originalHost, originalPort, err := splitHostPort(target)
	if err != nil {
		return err
	}
	connectHost := resolveWithDoH(context.Background(), originalHost, NezhaDoHEndpoints)
	if connectHost == "" {
		connectHost = originalHost
	}
	addr := formatHostPort(connectHost, originalPort)

	var creds credentials.TransportCredentials
	if NezhaTLS {
		creds = credentials.NewTLS(&tls.Config{ServerName: originalHost})
	} else {
		creds = insecure.NewCredentials()
	}

	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithAuthority(originalHost),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewNezhaServiceClient(conn)
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := metadata.AppendToOutgoingContext(baseCtx, "client_secret", NezhaKey, "client_uuid", UUID)

	receipt, err := client.ReportSystemInfo2(ctx, collectHost())
	if err != nil {
		return fmt.Errorf("ReportSystemInfo2: %w", err)
	}
	dashboardBootTime = receipt.Data

	errCh := make(chan error, 2)
	go reportHostLoop(ctx, client)
	go geoIPReportLoop(ctx, client)
	go func() { errCh <- runTaskStream(ctx, client) }()
	go func() { errCh <- runStateStream(ctx, client) }()

	err = <-errCh
	cancel()
	return err
}

func runStateStream(ctx context.Context, client pb.NezhaServiceClient) error {
	stream, err := client.ReportSystemState(ctx)
	if err != nil {
		return fmt.Errorf("ReportSystemState: %w", err)
	}
	recvDone := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				recvDone <- err
				return
			}
		}
	}()

	delay := time.Duration(NezhaReportDelay) * time.Second
	for {
		if err := stream.Send(collectState()); err != nil {
			return fmt.Errorf("state Send: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvDone:
			return fmt.Errorf("state Recv: %w", err)
		case <-time.After(delay):
		}
	}
}

const (
	taskTypeHTTPGet      = 1
	taskTypeICMPPing     = 2
	taskTypeTCPPing      = 3
	taskTypeCommand      = 4
	taskTypeKeepalive    = 7
	taskTypeTerminalGRPC = 8
	taskTypeFM           = 11
	taskTypeReportConfig = 12
)

func runTaskStream(ctx context.Context, client pb.NezhaServiceClient) error {
	stream, err := client.RequestTask(ctx)
	if err != nil {
		return fmt.Errorf("RequestTask: %w", err)
	}
	resultCh := make(chan *pb.TaskResult, 32)
	sendErrCh := make(chan error, 1)
	go func() {
		for result := range resultCh {
			if err := stream.Send(result); err != nil {
				sendErrCh <- err
				return
			}
		}
	}()

	for {
		task, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("task Recv: %w", err)
		}
		select {
		case err := <-sendErrCh:
			return fmt.Errorf("task Send: %w", err)
		default:
		}
		go handleTask(ctx, client, task, resultCh)
	}
}

func handleTask(ctx context.Context, client pb.NezhaServiceClient, task *pb.Task, resultCh chan<- *pb.TaskResult) {
	result := &pb.TaskResult{Id: task.Id, Type: task.Type, Successful: false}
	switch task.Type {
	case taskTypeKeepalive:
		return
	case taskTypeHTTPGet:
		doHTTPGet(task, result)
	case taskTypeTCPPing:
		doTCPPing(task, result)
	case taskTypeICMPPing:
		doICMPPing(task, result)
	case taskTypeCommand:
		doCommand(task, result)
	case taskTypeTerminalGRPC:
		if streamID := streamIDFromTask(task.Data, "terminal"); streamID != "" {
			go startTerminalSession(ctx, client, streamID)
		}
		return
	case taskTypeFM:
		if streamID := streamIDFromTask(task.Data, "file manager"); streamID != "" {
			go startFileManagerSession(ctx, client, streamID)
		}
		return
	case taskTypeReportConfig:
		data, _ := json.Marshal(buildConfigReport())
		result.Data = string(data)
		result.Successful = true
	default:
		log.Printf("[NEZHA] Unsupported task type: %d", task.Type)
		return
	}
	resultCh <- result
}

func doHTTPGet(task *pb.Task, result *pb.TaskResult) {
	if NezhaDisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}
	start := time.Now()
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("GET", task.Data, nil)
	if err != nil {
		result.Data = err.Error()
		return
	}
	req.Header.Set("User-Agent", "OneImg-Go/1.0")
	resp, err := client.Do(req)
	if err != nil {
		result.Data = err.Error()
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	result.Delay = float32(time.Since(start).Milliseconds())
	if resp.StatusCode >= 200 && resp.StatusCode <= 399 {
		result.Successful = true
	} else {
		result.Data = fmt.Sprintf("HTTP error: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
}

func doTCPPing(task *pb.Task, result *pb.TaskResult) {
	if NezhaDisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", task.Data, 10*time.Second)
	if err != nil {
		result.Data = err.Error()
		return
	}
	conn.Close()
	result.Delay = float32(time.Since(start).Milliseconds())
	result.Successful = true
}

func doICMPPing(task *pb.Task, result *pb.TaskResult) {
	if NezhaDisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}
	pingPath, err := exec.LookPath("ping")
	if err != nil {
		result.Data = "ping command is not available"
		return
	}
	args := []string{"-c", "5", "-W", "4", task.Data}
	if runtime.GOOS == "windows" {
		args = []string{"-n", "5", "-w", "4000", task.Data}
	}
	start := time.Now()
	output, err := withTimeout(exec.Command(pingPath, args...), 25*time.Second)
	result.Delay = float32(time.Since(start).Milliseconds() / 5)
	text := string(output)
	if err == nil {
		result.Successful = true
		result.Data = tail(text, 2048)
	} else {
		result.Data = tail(text, 4096)
		if result.Data == "" {
			result.Data = fmt.Sprintf("ping exited: %v", err)
		}
	}
}

func doCommand(task *pb.Task, result *pb.TaskResult) {
	if NezhaDisableCommandExecute {
		result.Data = "This agent has disabled command execution"
		return
	}
	start := time.Now()
	cmd := shellCommand(task.Data)
	output, err := withTimeout(cmd, 2*time.Hour)
	result.Delay = float32(time.Since(start).Seconds())
	data := tail(string(output), 2*1024*1024)
	if err == nil {
		result.Data = data
		result.Successful = true
	} else {
		result.Data = data + fmt.Sprintf("\nexit: %v", err)
	}
}

func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", command)
	}
	return exec.Command("sh", "-c", command)
}

func withTimeout(cmd *exec.Cmd, timeout time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.Bytes(), err
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		return buf.Bytes(), fmt.Errorf("timeout")
	}
}

func tail(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[len(text)-max:]
}

func streamIDFromTask(data string, label string) string {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		log.Printf("[NEZHA] Invalid %s task payload", label)
		return ""
	}
	for _, key := range []string{"StreamID", "stream_id", "streamId"} {
		if value, ok := payload[key]; ok && value != nil {
			streamID := strings.TrimSpace(fmt.Sprintf("%v", value))
			if streamID != "" {
				return streamID
			}
		}
	}
	log.Printf("[NEZHA] %s task missing StreamID", label)
	return ""
}

var ioStreamPrefix = []byte{0xff, 0x05, 0xff, 0x05}

func startTerminalSession(ctx context.Context, client pb.NezhaServiceClient, streamID string) {
	if runtime.GOOS != "linux" {
		log.Printf("[NEZHA] Terminal requires a Unix PTY environment")
		return
	}
	stream, err := client.IOStream(ctx)
	if err != nil {
		log.Printf("[NEZHA] Terminal IOStream error: %v", err)
		return
	}
	reg := append(append([]byte{}, ioStreamPrefix...), []byte(streamID)...)
	if err := stream.Send(&pb.IOStreamData{Data: reg}); err != nil {
		return
	}
	runTerminalSession(stream)
}

const (
	fmChunkSize = 1024 * 1024
)

var (
	fmComplete = []byte("NZUP")
	fmFile     = []byte("NZTD")
	fmFileName = []byte("NZFN")
	fmError    = []byte("NERR")
)

func startFileManagerSession(ctx context.Context, client pb.NezhaServiceClient, streamID string) {
	stream, err := client.IOStream(ctx)
	if err != nil {
		log.Printf("[NEZHA] FM IOStream error: %v", err)
		return
	}
	var sendMu sync.Mutex
	send := func(data []byte) {
		sendMu.Lock()
		defer sendMu.Unlock()
		stream.Send(&pb.IOStreamData{Data: data})
	}
	reg := append(append([]byte{}, ioStreamPrefix...), []byte(streamID)...)
	send(reg)

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				send(nil)
			}
		}
	}()

	var uploadFile *os.File
	var uploadSize, uploadReceived uint64
	resetUpload := func() {
		if uploadFile != nil {
			uploadFile.Close()
		}
		uploadFile = nil
		uploadSize = 0
		uploadReceived = 0
	}
	defer resetUpload()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return
		}
		payload := msg.Data
		if len(payload) == 0 {
			continue
		}
		if uploadFile != nil {
			if _, err := uploadFile.Write(payload); err != nil {
				send(fmErrorPayload(err.Error()))
				resetUpload()
				continue
			}
			uploadReceived += uint64(len(payload))
			if uploadReceived >= uploadSize {
				resetUpload()
				send(append([]byte{}, fmComplete...))
			}
			continue
		}

		switch payload[0] {
		case 0:
			fmListDir(send, fmPathFrom(payload, 1))
		case 1:
			fmDownload(send, fmPathFrom(payload, 1))
		case 2:
			if len(payload) < 9 {
				send(fmErrorPayload("data is invalid"))
				continue
			}
			uploadSize = binary.BigEndian.Uint64(payload[1:9])
			uploadReceived = 0
			uploadPath := fmPathFrom(payload, 9)
			if uploadPath == "" {
				send(fmErrorPayload("path is empty"))
				resetUpload()
				continue
			}
			if parent := filepath.Dir(uploadPath); parent != "" && parent != "." {
				os.MkdirAll(parent, 0755)
			}
			uploadFile, err = os.Create(uploadPath)
			if err != nil {
				send(fmErrorPayload(err.Error()))
				resetUpload()
				continue
			}
			if uploadSize == 0 {
				resetUpload()
				send(append([]byte{}, fmComplete...))
			}
		default:
			send(fmErrorPayload(fmt.Sprintf("unknown file manager opcode: %d", payload[0])))
		}
	}
}

func fmListDir(send func([]byte), requested string) {
	directory := requested
	if directory == "" || !isDir(directory) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = string(os.PathSeparator)
		}
		directory = home
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		send(fmErrorPayload(err.Error()))
		return
	}
	displayPath := abs + string(os.PathSeparator)
	pathBytes := []byte(displayPath)
	payload := append([]byte{}, fmFileName...)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pathBytes)))
	payload = append(payload, lenBuf[:]...)
	payload = append(payload, pathBytes...)

	entries, err := os.ReadDir(directory)
	if err != nil {
		send(fmErrorPayload(err.Error()))
		return
	}
	for _, entry := range entries {
		nameBytes := []byte(entry.Name())
		isDirByte := byte(0)
		if entry.IsDir() {
			isDirByte = 1
		}
		payload = append(payload, isDirByte, byte(len(nameBytes)&0xff))
		payload = append(payload, nameBytes...)
	}
	send(payload)
}

func fmDownload(send func([]byte), path string) {
	if path == "" {
		send(fmErrorPayload("path is empty"))
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		send(fmErrorPayload(err.Error()))
		return
	}
	if info.Size() <= 0 {
		send(fmErrorPayload("requested file is empty"))
		return
	}
	if !info.Mode().IsRegular() {
		send(fmErrorPayload("requested path is not a file"))
		return
	}
	file, err := os.Open(path)
	if err != nil {
		send(fmErrorPayload(err.Error()))
		return
	}
	defer file.Close()

	header := append([]byte{}, fmFile...)
	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], uint64(info.Size()))
	header = append(header, sizeBuf[:]...)
	send(header)

	buf := make([]byte, fmChunkSize)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			send(chunk)
		}
		if err != nil {
			break
		}
	}
}

func fmErrorPayload(message string) []byte {
	return append(append([]byte{}, fmError...), []byte(message)...)
}

func fmPathFrom(payload []byte, offset int) string {
	if len(payload) <= offset {
		return ""
	}
	return string(payload[offset:])
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func reportHostLoop(ctx context.Context, client pb.NezhaServiceClient) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if receipt, err := client.ReportSystemInfo2(ctx, collectHost()); err == nil {
				dashboardBootTime = receipt.Data
			}
		}
	}
}

func collectHost() *pb.Host {
	platformName := runtime.GOOS
	platformVersion := runtime.GOOS
	cpuModel := runtime.GOARCH

	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			info := map[string]string{}
			for _, line := range strings.Split(string(data), "\n") {
				if idx := strings.IndexByte(line, '='); idx >= 0 {
					key := strings.TrimSpace(line[:idx])
					value := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
					info[key] = value
				}
			}
			if value := info["ID"]; value != "" {
				platformName = value
			}
			if value := info["PRETTY_NAME"]; value != "" {
				platformVersion = value
			}
		}
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "model name") {
					if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
						cpuModel = strings.TrimSpace(parts[1])
					}
					break
				}
			}
		}
	}

	vmem, _ := mem.VirtualMemory()
	swap, _ := mem.SwapMemory()
	host := &pb.Host{
		Platform:        platformName,
		PlatformVersion: platformVersion,
		Cpu:             []string{cpuModel},
		Arch:            runtime.GOARCH,
		Virtualization:  "",
		BootTime:        bootTime,
		Version:         nezhaAgentVersion,
	}
	if vmem != nil {
		host.MemTotal = vmem.Total
	}
	if swap != nil {
		host.SwapTotal = swap.Total
	}
	host.DiskTotal = getDiskTotal()
	return host
}

func collectState() *pb.State {
	cpuPercents, _ := cpu.Percent(0, false)
	var cpuUsage float64
	if len(cpuPercents) > 0 {
		cpuUsage = cpuPercents[0]
	}
	vmem, _ := mem.VirtualMemory()
	swap, _ := mem.SwapMemory()
	netIn, netOut, netInSpeed, netOutSpeed := getNetworkStats()
	load1, load5, load15 := getLoadAverage()
	state := &pb.State{
		Cpu:            cpuUsage,
		DiskUsed:       getDiskUsed(),
		NetInTransfer:  netIn,
		NetOutTransfer: netOut,
		NetInSpeed:     netInSpeed,
		NetOutSpeed:    netOutSpeed,
		Uptime:         uint64(time.Now().Unix()) - bootTime,
		Load1:          load1,
		Load5:          load5,
		Load15:         load15,
	}
	if vmem != nil {
		state.MemUsed = vmem.Total - vmem.Available
	}
	if swap != nil {
		state.SwapUsed = swap.Used
	}
	if !NezhaSkipConnectionCount {
		state.TcpConnCount, state.UdpConnCount = getConnectionCounts()
	}
	if !NezhaSkipProcsCount {
		state.ProcessCount = getProcessCount()
	}
	return state
}

func getLoadAverage() (float64, float64, float64) {
	avg, err := load.Avg()
	if err != nil || avg == nil {
		return 0, 0, 0
	}
	return avg.Load1, avg.Load5, avg.Load15
}

func getNetworkStats() (netIn, netOut, netInSpeed, netOutSpeed uint64) {
	ios, err := psnet.IOCounters(false)
	if err != nil || len(ios) == 0 {
		return lastNetIn, lastNetOut, 0, 0
	}
	curIn := ios[0].BytesRecv
	curOut := ios[0].BytesSent
	now := time.Now().Unix()
	if lastNetTime > 0 {
		diff := uint64(now - lastNetTime)
		if diff > 0 {
			if curIn >= lastNetIn {
				netInSpeed = (curIn - lastNetIn) / diff
			}
			if curOut >= lastNetOut {
				netOutSpeed = (curOut - lastNetOut) / diff
			}
		}
	}
	lastNetIn = curIn
	lastNetOut = curOut
	lastNetTime = now
	return curIn, curOut, netInSpeed, netOutSpeed
}

func getConnectionCounts() (uint64, uint64) {
	tcpConns, _ := psnet.Connections("tcp")
	udpConns, _ := psnet.Connections("udp")
	return uint64(len(tcpConns)), uint64(len(udpConns))
}

func getProcessCount() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	var count uint64
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			count++
		}
	}
	return count
}

func getDiskTotal() uint64 {
	total, _ := diskTotals()
	return total
}

func getDiskUsed() uint64 {
	_, used := diskTotals()
	return used
}

func diskTotals() (uint64, uint64) {
	parts, err := disk.Partitions(false)
	if err != nil || len(parts) == 0 {
		if usage, err := disk.Usage("."); err == nil && usage != nil {
			return usage.Total, usage.Used
		}
		return 0, 0
	}
	seen := map[string]bool{}
	var total uint64
	var used uint64
	for _, part := range parts {
		key := part.Device
		if key == "" {
			key = part.Mountpoint
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		usage, err := disk.Usage(part.Mountpoint)
		if err != nil || usage == nil {
			continue
		}
		total += usage.Total
		used += usage.Used
	}
	return total, used
}

func geoIPReportLoop(ctx context.Context, client pb.NezhaServiceClient) {
	var lastIP string
	for {
		geoIP, selected := fetchGeoIP()
		if geoIP != nil && selected != "" && selected != lastIP {
			lastIP = selected
			if _, err := client.ReportGeoIP(ctx, geoIP); err != nil {
				log.Printf("[NEZHA] GeoIP report error: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(NezhaIPReportPeriod) * time.Second):
		}
	}
}

func fetchGeoIP() (*pb.GeoIP, string) {
	ipv4, ipv6 := fetchPublicIP()
	selected := ipv4
	if NezhaUseIPv6CountryCode && ipv6 != "" {
		selected = ipv6
	}
	if selected == "" {
		selected = ipv6
	}
	if selected == "" {
		return nil, ""
	}
	return &pb.GeoIP{
		Ip:                &pb.IP{Ipv4: ipv4, Ipv6: ipv6},
		Use6:              NezhaUseIPv6CountryCode,
		DashboardBootTime: dashboardBootTime,
	}, selected
}

func fetchPublicIP() (ipv4, ipv6 string) {
	endpoints := []string{
		"https://blog.cloudflare.com/cdn-cgi/trace",
		"https://developers.cloudflare.com/cdn-cgi/trace",
		"https://hostinger.com/cdn-cgi/trace",
		"https://ahrefs.com/cdn-cgi/trace",
	}
	client := &http.Client{Timeout: 20 * time.Second}
	for _, endpoint := range endpoints {
		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "OneImg-Go/1.0")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		candidate := extractIP(string(bodyBytes))
		if candidate == "" {
			continue
		}
		parsed := net.ParseIP(candidate)
		if parsed == nil {
			continue
		}
		if strings.Contains(candidate, ":") {
			if ipv6 == "" {
				ipv6 = candidate
			}
		} else if ipv4 == "" {
			ipv4 = candidate
		}
		if ipv4 != "" && ipv6 != "" {
			break
		}
	}
	return
}

func extractIP(body string) string {
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "ip=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ip="))
		}
	}
	return strings.TrimSpace(body)
}

func buildConfigReport() map[string]interface{} {
	return map[string]interface{}{
		"debug":                   Debug,
		"server":                  resolveNezhaTarget(NezhaServer, NezhaPort),
		"client_secret":           NezhaKey,
		"uuid":                    UUID,
		"tls":                     NezhaTLS,
		"report_delay":            NezhaReportDelay,
		"ip_report_period":        NezhaIPReportPeriod,
		"skip_connection_count":   NezhaSkipConnectionCount,
		"skip_procs_count":        NezhaSkipProcsCount,
		"disable_command_execute": NezhaDisableCommandExecute,
		"disable_send_query":      NezhaDisableSendQuery,
		"disable_nat":             NezhaDisableNat,
		"gpu":                     false,
		"temperature":             false,
		"disable_auto_update":     true,
		"disable_force_update":    true,
		"use_ipv6_country_code":   NezhaUseIPv6CountryCode,
		"doh":                     strings.Join(NezhaDoHEndpoints, ","),
	}
}
