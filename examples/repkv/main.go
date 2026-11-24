// repkv is a tiny three-node replicated KV for Fuss isolate scenarios.
// Each node has a client HTTP port (PUT/GET /k/...) and a peer TCP port.
// Peer traffic is meant to go through Fuss proxies; client ports stay direct.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const electionTimeout = 250 * time.Millisecond

func main() {
	drive := flag.Bool("drive", false, "PUT then fire isolate trigger")
	verify := flag.Bool("verify", false, "GET key from new leader among nodes 1..n-1")
	id := flag.Int("id", 0, "node index")
	dataDir := flag.String("data-dir", "", "node data directory")
	peerListen := flag.String("peer-listen", "", "peer listen addr (real port)")
	peerAddrs := flag.String("peer-addrs", "", "comma list of peer dial addrs by index (empty=self)")
	client := flag.String("client", "127.0.0.1:0", "client HTTP listen")
	mode := flag.String("mode", "correct", "correct|bug")
	gate := flag.String("gate", "", "bug mode: hold replication until this file exists")
	dir := flag.String("dir", "", "trial dir (drive/verify)")
	n := flag.Int("n", 3, "node count (drive/verify)")
	key := flag.String("key", "", "key (drive/verify)")
	record := flag.String("record", "", "ack record path (drive)")
	cut := flag.String("cut", "", "isolate trigger file (drive)")
	flag.Parse()

	switch {
	case *drive:
		os.Exit(runDrive(*dir, *n, *key, *record, *cut, *gate, *mode))
	case *verify:
		os.Exit(runVerify(*dir, *n, *key))
	default:
		if *dataDir == "" || *peerListen == "" {
			fmt.Fprintln(os.Stderr, "repkv: -data-dir and -peer-listen required")
			os.Exit(2)
		}
		os.Exit(runNode(*id, *dataDir, *peerListen, *peerAddrs, *client, *mode, *gate))
	}
}

type node struct {
	id       int
	mode     string
	gate     string
	peers    []string // index -> dial addr; peers[id] == ""
	mu       sync.Mutex
	store    map[string]string
	leader   int
	lastHB   time.Time
	term     int
	pending  map[string]string // bug mode: waiting for gate
}

func runNode(id int, dataDir, peerListen, peerAddrs, client, mode, gate string) int {
	_ = os.MkdirAll(dataDir, 0o755)
	peers := strings.Split(peerAddrs, ",")
	for len(peers) <= id {
		peers = append(peers, "")
	}
	n := &node{
		id: id, mode: mode, gate: gate, peers: peers,
		store: map[string]string{}, pending: map[string]string{},
		leader: 0, lastHB: time.Now(),
	}
	// Node 0 starts as leader; others follow until heartbeats stop.
	if id != 0 {
		n.leader = 0
	} else {
		n.leader = 0
	}

	pln, err := net.Listen("tcp", peerListen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go acceptPeers(pln, n)

	cln, err := net.Listen("tcp", client)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = os.WriteFile(filepath.Join(dataDir, "client"), []byte(cln.Addr().String()+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dataDir, "ready"), []byte("READY\n"), 0o644)

	go n.electionLoop()
	go n.heartbeatLoop()
	if mode == "bug" {
		go n.gateLoop()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/k/", n.handleK)
	mux.HandleFunc("/leader", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		lead := n.leader
		n.mu.Unlock()
		fmt.Fprintf(w, "%d\n", lead)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "OK\n")
	})
	fmt.Fprintln(os.Stderr, http.Serve(cln, mux))
	return 1
}

func (n *node) handleK(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/k/")
	switch r.Method {
	case http.MethodGet:
		n.mu.Lock()
		v, ok := n.store[key]
		n.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, v)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		val := string(body)
		n.mu.Lock()
		isLeader := n.leader == n.id
		n.mu.Unlock()
		if !isLeader {
			http.Error(w, "not leader", http.StatusServiceUnavailable)
			return
		}
		if n.mode == "bug" {
			n.mu.Lock()
			n.store[key] = val
			n.pending[key] = val
			n.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		// correct: ack only after at least one follower confirms; we ask all.
		if !n.replicateAll(key, val) {
			http.Error(w, "replicate failed", http.StatusServiceUnavailable)
			return
		}
		n.mu.Lock()
		n.store[key] = val
		n.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (n *node) replicateAll(key, val string) bool {
	okN := 0
	for j, addr := range n.peers {
		if j == n.id || addr == "" {
			continue
		}
		if sendRepl(addr, key, val) {
			okN++
		}
	}
	return okN >= 1
}

func sendRepl(addr, key, val string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = fmt.Fprintf(c, "REPL %s %s\n", key, val)
	if err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.TrimSpace(line) == "REPL_OK"
}

func acceptPeers(ln net.Listener, n *node) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handlePeer(c, n)
	}
}

func handlePeer(c net.Conn, n *node) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "HB":
		// HB <leaderId>
		if len(parts) >= 2 {
			lead, _ := strconv.Atoi(parts[1])
			n.mu.Lock()
			n.leader = lead
			n.lastHB = time.Now()
			n.mu.Unlock()
		}
		_, _ = io.WriteString(c, "OK\n")
	case "REPL":
		if len(parts) < 3 {
			return
		}
		key, val := parts[1], strings.Join(parts[2:], " ")
		n.mu.Lock()
		n.store[key] = val
		n.mu.Unlock()
		_, _ = io.WriteString(c, "REPL_OK\n")
	case "PING":
		_, _ = io.WriteString(c, "PONG\n")
	case "WHO":
		n.mu.Lock()
		lead := n.leader
		n.mu.Unlock()
		fmt.Fprintf(c, "ID %d LEADER %d\n", n.id, lead)
	}
}

// electionLoop: if we lose the leader for electionTimeout, the lowest-numbered
// reachable node (self included) becomes leader. Reachable means a peer PING
// through the configured dial addr succeeds. Isolation cuts those dials, so an
// isolated node 0 cannot win among {1,2}, and 1 wins as min(1,2).
func (n *node) electionLoop() {
	for {
		time.Sleep(50 * time.Millisecond)
		n.mu.Lock()
		lead := n.leader
		since := time.Since(n.lastHB)
		n.mu.Unlock()
		if lead == n.id {
			continue
		}
		if since < electionTimeout {
			continue
		}
		reachable := []int{n.id}
		for j, addr := range n.peers {
			if j == n.id || addr == "" {
				continue
			}
			if pingPeer(addr) {
				reachable = append(reachable, j)
			}
		}
		winner := reachable[0]
		for _, id := range reachable[1:] {
			if id < winner {
				winner = id
			}
		}
		n.mu.Lock()
		n.leader = winner
		if winner == n.id {
			n.lastHB = time.Now()
		}
		n.mu.Unlock()
	}
}

func pingPeer(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = io.WriteString(c, "PING\n")
	if err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.TrimSpace(line) == "PONG"
}

func (n *node) heartbeatLoop() {
	for {
		time.Sleep(50 * time.Millisecond)
		n.mu.Lock()
		isLeader := n.leader == n.id
		n.mu.Unlock()
		if !isLeader {
			continue
		}
		for j, addr := range n.peers {
			if j == n.id || addr == "" {
				continue
			}
			sendHB(addr, n.id)
		}
		n.mu.Lock()
		n.lastHB = time.Now()
		n.mu.Unlock()
	}
}

func sendHB(addr string, leaderID int) {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = fmt.Fprintf(c, "HB %d\n", leaderID)
	_, _ = bufio.NewReader(c).ReadString('\n')
}

func (n *node) gateLoop() {
	for {
		time.Sleep(50 * time.Millisecond)
		if _, err := os.Stat(n.gate); err != nil {
			continue
		}
		n.mu.Lock()
		pending := n.pending
		n.pending = map[string]string{}
		n.mu.Unlock()
		for k, v := range pending {
			_ = n.replicateAll(k, v)
		}
	}
}

func runDrive(dir string, n int, key, record, cut, gate, mode string) int {
	// Wait for every node's client addr file.
	clients := make([]string, n)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for i := 0; i < n; i++ {
			b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("node%d", i), "client"))
			if err != nil {
				ok = false
				break
			}
			clients[i] = strings.TrimSpace(string(b))
		}
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, c := range clients {
		if c == "" {
			fmt.Fprintf(os.Stderr, "drive: no client for node %d\n", i)
			return 1
		}
	}
	// Wait until node 0 is leader and accepting.
	deadline = time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get("http://" + clients[0] + "/leader")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.TrimSpace(string(b)) == "0" {
				break
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "drive: node 0 not leader")
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}

	req, _ := http.NewRequest(http.MethodPut, "http://"+clients[0]+"/k/"+key, strings.NewReader("v"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		fmt.Fprintf(os.Stderr, "drive: PUT failed: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if record != "" {
		f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "ack %s\n", key)
			f.Close()
		}
	}
	_ = os.WriteFile(cut, []byte("NOW\n"), 0o644)
	if mode == "bug" && gate != "" {
		// Let isolate land, then release the held replication (too late).
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(gate, []byte("GO\n"), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "drive_done"), []byte("DONE\n"), 0o644)
	return 0
}

func runVerify(dir string, n int, key string) int {
	clients := make([]string, n)
	for i := 0; i < n; i++ {
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("node%d", i), "client"))
		if err != nil {
			fmt.Println("UNKNOWN")
			return 0
		}
		clients[i] = strings.TrimSpace(string(b))
	}
	// Wait for a leader among nodes 1..n-1 (not the isolated node 0).
	deadline := time.Now().Add(3 * time.Second)
	newLead := -1
	for time.Now().Before(deadline) {
		for i := 1; i < n; i++ {
			resp, err := http.Get("http://" + clients[i] + "/leader")
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lead, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && lead >= 1 && lead < n {
				// Confirm that node agrees it is leader / serves.
				newLead = lead
				break
			}
		}
		if newLead >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if newLead < 1 {
		fmt.Println("UNKNOWN")
		return 0
	}
	resp, err := http.Get("http://" + clients[newLead] + "/k/" + key)
	if err != nil {
		fmt.Println("UNKNOWN")
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		fmt.Println("PASS")
		return 0
	}
	fmt.Println("LOST WRITE")
	return 0
}
