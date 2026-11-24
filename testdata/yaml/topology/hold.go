// Tiny topology fixture: write a ready marker under data_dir, then sleep.
// Optional second arg "never" skips the marker so readiness times out.
// With a listen port and peer addrs: accept PING/PONG and ping peers,
// writing data_dir/pings and data_dir/ping_ok.
package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	dir := os.Args[1]
	_ = os.MkdirAll(dir, 0o755)

	if len(os.Args) >= 3 && os.Args[2] == "never" {
		time.Sleep(30 * time.Second)
		return
	}

	if len(os.Args) >= 3 {
		port := os.Args[2]
		var peers []string
		for _, a := range os.Args[3:] {
			for _, p := range strings.Split(a, ",") {
				if p != "" {
					peers = append(peers, p)
				}
			}
		}
		runPeer(dir, port, peers)
		return
	}

	_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("READY\n"), 0o644)
	time.Sleep(30 * time.Second)
}

func runPeer(dir, port string, peers []string) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(1)
	}
	defer ln.Close()
	go acceptPings(ln)

	_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("READY\n"), 0o644)

	for {
		var lines []string
		allOK := len(peers) > 0
		for _, p := range peers {
			if p == "" {
				continue
			}
			if pingOnce(p) {
				lines = append(lines, p+" ok")
			} else {
				lines = append(lines, p+" fail")
				allOK = false
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "pings"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		if allOK {
			_ = os.WriteFile(filepath.Join(dir, "ping_ok"), []byte("OK\n"), 0o644)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func acceptPings(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			r := bufio.NewReader(c)
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "PING" {
				_, _ = c.Write([]byte("PONG\n"))
			}
		}(c)
	}
}

func pingOnce(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.Write([]byte("PING\n")); err != nil {
		return false
	}
	buf := make([]byte, 5)
	n, err := c.Read(buf)
	return err == nil && string(buf[:n]) == "PONG\n"
}
