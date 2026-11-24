package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// OwnerPID returns the pid in pids that owns the client socket of an accepted
// loopback TCP connection. remote is that connection's remote address.
// It returns an error when that owner is not exactly one of pids.
func OwnerPID(remote net.Addr, pids []int) (int, error) {
	port, err := addrPort(remote)
	if err != nil {
		return 0, err
	}
	pids = normalizePIDs(pids)
	switch runtime.GOOS {
	case "linux":
		return ownerLinux(port, pids)
	case "darwin":
		return ownerDarwin(port, pids)
	default:
		return 0, fmt.Errorf("proxy: OwnerPID is not supported on %s", runtime.GOOS)
	}
}

func addrPort(remote net.Addr) (int, error) {
	if remote == nil {
		return 0, fmt.Errorf("proxy: nil remote address")
	}
	if ta, ok := remote.(*net.TCPAddr); ok {
		if ta == nil {
			return 0, fmt.Errorf("proxy: nil remote address")
		}
		if ta.Port <= 0 || ta.Port > 65535 {
			return 0, fmt.Errorf("proxy: remote address %q has no TCP port", ta.String())
		}
		return ta.Port, nil
	}
	_, portStr, err := net.SplitHostPort(remote.String())
	if err != nil {
		return 0, fmt.Errorf("proxy: remote address %q: %w", remote.String(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("proxy: remote address %q has no TCP port", remote.String())
	}
	return port, nil
}

func normalizePIDs(pids []int) []int {
	if len(pids) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(pids))
	out := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		out = append(out, pid)
	}
	return out
}

func oneOwner(found []int, port int) (int, error) {
	sort.Ints(found)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return 0, fmt.Errorf("proxy: no PID owns local port %d", port)
	default:
		return 0, fmt.Errorf("proxy: multiple PIDs own local port %d: %v", port, found)
	}
}

func ownerLinux(port int, pids []int) (int, error) {
	inodes, err := inodesForLocalPort(port)
	if err != nil {
		return 0, err
	}
	if len(inodes) == 0 {
		return 0, fmt.Errorf("proxy: no PID owns local port %d", port)
	}
	var found []int
	for _, pid := range pids {
		owns, err := pidOwnsInode(pid, inodes)
		if err != nil {
			return 0, err
		}
		if owns {
			found = append(found, pid)
		}
	}
	return oneOwner(found, port)
}

// inodesForLocalPort returns socket inodes whose local port is port.
// /proc/net/tcp and tcp6 share a column layout: tx_queue/rx_queue is one
// field and tr/tm->when is one field, so the inode is field 9.
func inodesForLocalPort(port int) (map[uint64]struct{}, error) {
	inodes := make(map[uint64]struct{})
	scanned := 0
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		n, err := scanProcNet(path, port, inodes)
		if err != nil {
			return nil, err
		}
		if n >= 0 {
			scanned++
		}
	}
	if scanned == 0 {
		return nil, fmt.Errorf("proxy: cannot read /proc/net/tcp or /proc/net/tcp6")
	}
	return inodes, nil
}

// scanProcNet adds matching inodes. It returns -1 when path does not exist.
func scanProcNet(path string, port int, inodes map[uint64]struct{}) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return 0, fmt.Errorf("proxy: read %s: %w", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		localPort, ok := hexPort(fields[1])
		if !ok || localPort != port {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		inodes[inode] = struct{}{}
		n++
	}
	return n, nil
}

func hexPort(addr string) (int, bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return 0, false
	}
	n, err := strconv.ParseUint(addr[i+1:], 16, 16)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

func pidOwnsInode(pid int, inodes map[uint64]struct{}) (bool, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("proxy: read %s: %w", dir, err)
	}
	for _, ent := range entries {
		link, err := os.Readlink(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		inode, ok := socketInode(link)
		if !ok {
			continue
		}
		if _, owns := inodes[inode]; owns {
			return true, nil
		}
	}
	return false, nil
}

func socketInode(link string) (uint64, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, "]") {
		return 0, false
	}
	inode, err := strconv.ParseUint(link[len(prefix):len(link)-1], 10, 64)
	if err != nil || inode == 0 {
		return 0, false
	}
	return inode, true
}

func ownerDarwin(port int, pids []int) (int, error) {
	if len(pids) == 0 {
		return 0, fmt.Errorf("proxy: no PID owns local port %d", port)
	}
	cmd := exec.Command("lsof", "-nP", "-a", "-iTCP", "-p", joinPIDs(pids))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return 0, lsofErr(err, stderr.String())
		}
	}
	found := ownersFromLsof(stdout.String(), port, pids)
	if len(found) == 0 {
		if msg := strings.TrimSpace(stderr.String()); msg != "" && err != nil {
			return 0, fmt.Errorf("proxy: lsof: %s", msg)
		}
	}
	return oneOwner(found, port)
}

func lsofErr(err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		return fmt.Errorf("proxy: lsof: %w", err)
	}
	return fmt.Errorf("proxy: lsof: %w: %s", err, msg)
}

func joinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ",")
}

func ownersFromLsof(out string, port int, pids []int) []int {
	allowed := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		allowed[pid] = struct{}{}
	}
	seen := make(map[int]struct{})
	var found []int
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if _, ok := allowed[pid]; !ok {
			continue
		}
		local, ok := lsofLocalPort(fields)
		if !ok || local != port {
			continue
		}
		if _, dup := seen[pid]; dup {
			continue
		}
		seen[pid] = struct{}{}
		found = append(found, pid)
	}
	return found
}

func lsofLocalPort(fields []string) (int, bool) {
	for _, field := range fields {
		arrow := strings.Index(field, "->")
		if arrow < 0 {
			continue
		}
		_, portStr, err := net.SplitHostPort(field[:arrow])
		if err != nil {
			return 0, false
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return 0, false
		}
		return port, true
	}
	return 0, false
}
