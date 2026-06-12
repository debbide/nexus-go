//go:build linux
// +build linux

package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	pb "github.com/nezhahq/agent/proto"
)

func runTerminalSession(stream pb.NezhaService_IOStreamClient) {
	shell := findShell()
	cmd := exec.Command(shell, "-i")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[NEZHA] PTY start error: %v", err)
		return
	}
	defer ptmx.Close()

	var sendMu sync.Mutex
	send := func(data []byte) {
		sendMu.Lock()
		defer sendMu.Unlock()
		stream.Send(&pb.IOStreamData{Data: data})
	}

	done := make(chan struct{})
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-keepaliveDone:
				return
			case <-ticker.C:
				send(nil)
			}
		}
	}()

	go func() {
		defer close(done)
		buf := make([]byte, 10*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				send(data)
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				return
			}
			payload := msg.Data
			if len(payload) == 0 {
				continue
			}
			switch payload[0] {
			case 0:
				ptmx.Write(payload[1:])
			case 1:
				resizePTY(ptmx, payload[1:])
			default:
				ptmx.Write(payload)
			}
		}
	}()

	cmd.Wait()
	<-done
}

func resizePTY(ptmx *os.File, data []byte) {
	var payload struct {
		Cols uint16 `json:"Cols"`
		Rows uint16 `json:"Rows"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if payload.Cols == 0 || payload.Rows == 0 {
		return
	}
	pty.Setsize(ptmx, &pty.Winsize{Rows: payload.Rows, Cols: payload.Cols})
}

func findShell() string {
	for _, shell := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	return "/bin/sh"
}

var _ = syscall.SysProcAttr{}
