//go:build !linux
// +build !linux

package main

import (
	"log"

	pb "github.com/nezhahq/agent/proto"
)

func runTerminalSession(stream pb.NezhaService_IOStreamClient) {
	log.Printf("[NEZHA] Terminal session not supported on this platform")
	stream.Send(&pb.IOStreamData{Data: []byte("Terminal not supported on this platform\r\n")})
}

func findShell() string {
	return "/bin/sh"
}
