package proxy_test

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"fuss/proxy"
)

func TestMain(m *testing.M) {
	if addr := os.Getenv("PROXY_OWNER_DIAL"); addr != "" {
		dialAndHold(addr)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func dialAndHold(addr string) {
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer c.Close()
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestOwnerPID(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	c1, pid1 := startOwnerChild(t, ln)
	t.Cleanup(func() { _ = c1.Close() })
	c2, pid2 := startOwnerChild(t, ln)
	t.Cleanup(func() { _ = c2.Close() })
	if pid1 == pid2 {
		t.Fatalf("children share pid %d", pid1)
	}

	pids := []int{pid1, pid2}
	if got := ownerPID(t, c1.RemoteAddr(), pids); got != pid1 {
		t.Fatalf("conn1 owner=%d want %d", got, pid1)
	}
	if got := ownerPID(t, c2.RemoteAddr(), pids); got != pid2 {
		t.Fatalf("conn2 owner=%d want %d", got, pid2)
	}
	if _, err := proxy.OwnerPID(c1.RemoteAddr(), []int{pid2}); err == nil {
		t.Fatal("expected error when the owner pid is absent")
	}
}

func ownerPID(t *testing.T, remote net.Addr, pids []int) int {
	t.Helper()
	got, err := proxy.OwnerPID(remote, pids)
	if err != nil {
		t.Fatalf("OwnerPID(%s) pids %v: %v\n%s", remote, pids, err, lsofDump(pids))
	}
	return got
}

func lsofDump(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	out, err := exec.Command("lsof", "-nP", "-a", "-iTCP", "-p", strings.Join(parts, ",")).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("lsof: %v\n%s", err, out)
	}
	return string(out)
}

func startOwnerChild(t *testing.T, ln net.Listener) (net.Conn, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "PROXY_OWNER_DIAL="+ln.Addr().String())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(10 * time.Second))
		defer tl.SetDeadline(time.Time{})
	}
	c, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v (child stderr: %s)", err, stderr.String())
	}
	return c, cmd.Process.Pid
}
