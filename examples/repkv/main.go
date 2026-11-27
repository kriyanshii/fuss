// repkv is a tiny three-node replicated KV for Fuss scenarios.
// Each node has a client HTTP port (PUT/GET /k/...) and a peer TCP port.
// Peer traffic is meant to go through Fuss proxies. Client ports can too.
package main

import (
	"bufio"
	"encoding/json"
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
	inflight := flag.Bool("inflight", false, "replicate, cut the leader, and let the client time out before any response")
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
	cutNode := flag.String("cut-node", "node_0", "verify: isolate_applied.json target")
	readFault := flag.String("read-fault", "", "verify: refused dials a closed port after the leader is known")
	porcupineCheck := flag.Bool("porcupine", false, "verify: judge the recorded client history with Porcupine")
	schedule := flag.String("schedule", "", "before, pending, or after; when the cut is applied relative to the write")
	version := flag.Bool("version", false, "print repkv and exit")
	renderHistoryPath := flag.String("render-history", "", "print a saved history JSONL and exit")
	checkHistoryPath := flag.String("check-history", "", "decide whether a client history JSONL admits a legal order, then exit")
	initialValue := flag.String("initial", "0", "known initial value of the one key checked by -check-history")
	flag.Parse()

	if *version {
		fmt.Println("repkv")
		os.Exit(0)
	}
	if *renderHistoryPath != "" {
		b, err := os.ReadFile(*renderHistoryPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		text, err := renderHistory(b)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(text)
		os.Exit(0)
	}
	if *checkHistoryPath != "" {
		b, err := os.ReadFile(*checkHistoryPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if *schedule != "" {
			text := scheduleCheck(b, *initialValue, *schedule)
			fmt.Print(text)
			switch {
			case strings.HasPrefix(text, "experiment illegal"):
				os.Exit(1)
			case strings.HasPrefix(text, "experiment pass"), strings.HasPrefix(text, "experiment unknown"):
				os.Exit(0)
			default:
				os.Exit(2)
			}
		}
		if *porcupineCheck {
			text := formatHistoryCheck(b, *initialValue)
			fmt.Print(text)
			switch {
			case strings.HasPrefix(text, "history legal"):
				os.Exit(0)
			case strings.HasPrefix(text, "history illegal"):
				os.Exit(1)
			default:
				os.Exit(2)
			}
		}
		legal, text, err := checkHistory(b, *initialValue)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(text)
		if !legal {
			os.Exit(1)
		}
		os.Exit(0)
	}

	switch {
	case *verify:
		os.Exit(runVerify(*dir, *n, *key, *cutNode, *readFault, *porcupineCheck, *initialValue, *schedule))
	case *schedule != "":
		os.Exit(runSchedule(*dir, *n, *key, *cut, *gate, *schedule))
	case *inflight:
		os.Exit(runInflight(*dir, *n, *key, *cut, *gate))
	case *drive:
		os.Exit(runDrive(*dir, *n, *key, *record, *cut, *gate, *mode))
	default:
		if *dataDir == "" || *peerListen == "" {
			fmt.Fprintln(os.Stderr, "repkv: -data-dir and -peer-listen required")
			os.Exit(2)
		}
		os.Exit(runNode(*id, *dataDir, *peerListen, *peerAddrs, *client, *mode, *gate))
	}
}

type node struct {
	id      int
	mode    string
	gate    string
	peers   []string // index -> dial addr; peers[id] == ""
	mu      sync.Mutex
	writeMu sync.Mutex // one leader put at a time, so followers cannot reorder them
	store   map[string]string
	leader  int
	lastHB  time.Time
	term    int
	pending map[string]string // bug mode: waiting for gate
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
		// hold: the write is on every follower before the client is answered.
		// The handler then waits until the client gives up, so the response
		// is still outstanding when Fuss cuts the leader.
		if n.mode == "hold" {
			if !n.replicateEvery(key, val) {
				http.Error(w, "replicate failed", http.StatusServiceUnavailable)
				return
			}
			n.mu.Lock()
			n.store[key] = val
			n.mu.Unlock()
			if n.gate != "" {
				_ = os.WriteFile(n.gate, []byte("REPLICATED\n"), 0o644)
			}
			<-r.Context().Done()
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
		// correct: ack only after every follower has stored this value.
		if !n.commit(key, val) {
			http.Error(w, "replicate failed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (n *node) replicateEvery(key, val string) bool {
	need, okN := 0, 0
	for j, addr := range n.peers {
		if j == n.id || addr == "" {
			continue
		}
		need++
		if sendRepl(addr, n.id, key, val) {
			okN++
		}
	}
	return need > 0 && okN == need
}

// commit replicates val to every follower, then stores it locally.
// A partial replicate is rolled back so a failed ack cannot leak onto one peer.
func (n *node) commit(key, val string) bool {
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	if !n.replicateAll(key, val) {
		return false
	}
	n.mu.Lock()
	n.store[key] = val
	n.mu.Unlock()
	return true
}

func (n *node) replicateAll(key, val string) bool {
	n.mu.Lock()
	prev, had := n.store[key]
	lead := n.id
	n.mu.Unlock()
	var applied []string
	need := 0
	for j, addr := range n.peers {
		if j == n.id || addr == "" {
			continue
		}
		need++
		if sendRepl(addr, lead, key, val) {
			applied = append(applied, addr)
		}
	}
	if len(applied) == need {
		return true
	}
	for _, addr := range applied {
		if had {
			sendRepl(addr, lead, key, prev)
		} else {
			sendDel(addr, lead, key)
		}
	}
	return false
}

func sendRepl(addr string, leader int, key, val string) bool {
	// A follower can miss the first dial, or reject REPL until the next
	// heartbeat restores the leader id. One miss must not fail the commit:
	// the driver only opens the cut after this write is acknowledged.
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(40 * time.Millisecond)
		}
		if sendReplOnce(addr, leader, key, val) {
			return true
		}
	}
	return false
}

func sendReplOnce(addr string, leader int, key, val string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = fmt.Fprintf(c, "REPL %d %s %s\n", leader, key, val)
	if err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.TrimSpace(line) == "REPL_OK"
}

func sendDel(addr string, leader int, key string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = fmt.Fprintf(c, "DEL %d %s\n", leader, key)
	if err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.TrimSpace(line) == "DEL_OK"
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
		if len(parts) < 4 {
			return
		}
		lead, convErr := strconv.Atoi(parts[1])
		if convErr != nil {
			return
		}
		key, val := parts[2], strings.Join(parts[3:], " ")
		n.mu.Lock()
		cur := n.leader
		if cur == lead {
			n.store[key] = val
		}
		n.mu.Unlock()
		if cur != lead {
			_, _ = io.WriteString(c, "REPL_NO\n")
			return
		}
		_, _ = io.WriteString(c, "REPL_OK\n")
	case "DEL":
		if len(parts) < 3 {
			return
		}
		lead, convErr := strconv.Atoi(parts[1])
		if convErr != nil {
			return
		}
		key := parts[2]
		n.mu.Lock()
		cur := n.leader
		if cur == lead {
			delete(n.store, key)
		}
		n.mu.Unlock()
		if cur != lead {
			_, _ = io.WriteString(c, "DEL_NO\n")
			return
		}
		_, _ = io.WriteString(c, "DEL_OK\n")
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
			n.writeMu.Lock()
			_ = n.replicateAll(k, v)
			n.writeMu.Unlock()
		}
	}
}

// inflightClientTimeout is long enough for replication and the cut to
// finish while the PUT is still waiting, and short enough that the client
// then reports a timeout instead of a response.
const inflightClientTimeout = 2 * time.Second

// runSchedule is one write and one later read. The schedule names the gate
// that decides when cut_now is written, relative to that write.
func runSchedule(dir string, n int, key, cut, gate, schedule string) int {
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "drive_done"), []byte("DONE\n"), 0o644)
	}()
	clients, ok := waitClients(dir, n, 5*time.Second)
	if !ok {
		return 1
	}
	if !waitLeader(clients[0], 3*time.Second) {
		fmt.Fprintf(os.Stderr, "schedule %s: node 0 not leader\n", schedule)
		return 1
	}
	rec := openRecorder(filepath.Join(dir, "client.jsonl"))
	switch schedule {
	case "before":
		// The cut file is the gate. The write is invoked only after Fuss
		// has recorded the applied cut.
		_ = os.WriteFile(cut, []byte("NOW\n"), 0o644)
		_ = waitIsolateApplied(dir, "node_0", 10*time.Second)
		clientWrite(rec, clients[0], key, "v", 3*time.Second)
	case "pending":
		done := make(chan struct{})
		go func() {
			defer close(done)
			clientWrite(rec, clients[0], key, "v", inflightClientTimeout)
		}()
		if err := waitFile(gate, 5*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "schedule pending: replication not confirmed: %v\n", err)
			<-done
			return 0
		}
		_ = os.WriteFile(cut, []byte("NOW\n"), 0o644)
		<-done
		_ = waitIsolateApplied(dir, "node_0", 8*time.Second)
	case "after":
		// The acknowledgement is the gate. cut_now is written only after
		// the client has observed success.
		if clientWrite(rec, clients[0], key, "v", 3*time.Second) != "ok" {
			fmt.Fprintln(os.Stderr, "schedule after: write was not acknowledged")
			return 0
		}
		_ = os.WriteFile(cut, []byte("NOW\n"), 0o644)
		_ = waitIsolateApplied(dir, "node_0", 8*time.Second)
	default:
		fmt.Fprintf(os.Stderr, "schedule %q\n", schedule)
		return 1
	}
	return 0
}

func clientWrite(rec *recorder, addr, key, value string, timeout time.Duration) string {
	put := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPut, "http://"+addr+"/k/"+key, strings.NewReader(value))
	if err != nil {
		rec.start("w1", "write", key, value, "")
		rec.end("w1", "write", "error", "", err.Error(), false)
		return "error"
	}
	rec.start("w1", "write", key, value, "")
	resp, err := put.Do(req)
	status := 0
	detail := ""
	if resp != nil {
		status = resp.StatusCode
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		detail = strings.TrimSpace(string(b))
	}
	if err != nil {
		detail = err.Error()
	}
	obs := observeHTTP(err, status)
	rec.end("w1", "write", obs, "", detail, false)
	return obs
}

func runInflight(dir string, n int, key, cut, gate string) int {
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "drive_done"), []byte("DONE\n"), 0o644)
	}()
	clients, ok := waitClients(dir, n, 5*time.Second)
	if !ok {
		return 1
	}
	if !waitLeader(clients[0], 3*time.Second) {
		fmt.Fprintln(os.Stderr, "inflight: node 0 not leader")
		return 1
	}
	rec := openRecorder(filepath.Join(dir, "client.jsonl"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		put := &http.Client{Timeout: inflightClientTimeout}
		req, err := http.NewRequest(http.MethodPut, "http://"+clients[0]+"/k/"+key, strings.NewReader("v"))
		if err != nil {
			rec.end("w1", "write", "error", "", err.Error(), false)
			return
		}
		rec.start("w1", "write", key, "v", "")
		resp, err := put.Do(req)
		status := 0
		if resp != nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
		rec.end("w1", "write", observeHTTP(err, status), "", errText(err), false)
	}()
	if err := waitFile(gate, 5*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "inflight: replication not confirmed: %v\n", err)
		<-done
		return 0
	}
	_ = os.WriteFile(cut, []byte("NOW\n"), 0o644)
	<-done
	_ = waitIsolateApplied(dir, "node_0", 8*time.Second)
	return 0
}

func waitClients(dir string, n int, timeout time.Duration) ([]string, bool) {
	clients := make([]string, n)
	deadline := time.Now().Add(timeout)
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
			return clients, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, c := range clients {
		if c == "" {
			fmt.Fprintf(os.Stderr, "drive: no client for node %d\n", i)
		}
	}
	return nil, false
}

func waitLeader(client string, timeout time.Duration) bool {
	poll := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := poll.Get("http://" + client + "/leader")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.TrimSpace(string(b)) == "0" {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := os.Stat(path)
		if err == nil && st.Size() > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not written", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runDrive(dir string, n int, key, record, cut, gate, mode string) int {
	// Always release the trial. A stuck dial must not hold the kill file
	// until Fuss treats the run as a broken setup.
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "drive_done"), []byte("DONE\n"), 0o644)
	}()
	poll := &http.Client{Timeout: 300 * time.Millisecond}
	put := &http.Client{Timeout: 3 * time.Second}
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
		resp, err := poll.Get("http://" + clients[0] + "/leader")
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

	rec := openRecorder()
	// Retry a refused replicate. Under load one follower dial can miss, and
	// the cut file is written only after the ack. Giving up on the first
	// miss ends the trial with no partition.
	var status int
	var err error
	deadline = time.Now().Add(5 * time.Second)
	for {
		req, reqErr := http.NewRequest(http.MethodPut, "http://"+clients[0]+"/k/"+key, strings.NewReader("v"))
		if reqErr != nil {
			err = reqErr
			break
		}
		var resp *http.Response
		resp, err = put.Do(req)
		status = 0
		if resp != nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
		if err == nil && status == 200 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	rec.start("w1", "write", key, "v", "")
	rec.end("w1", "write", observeHTTP(err, status), "", errText(err), false)
	if err != nil || status != 200 {
		fmt.Fprintf(os.Stderr, "drive: PUT failed: %v status %d\n", err, status)
		return 1
	}
	if record != "" {
		f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "ack %s\n", key)
			f.Close()
		}
	}
	_ = os.WriteFile(cut, []byte("NOW\n"), 0o644)
	// Bug mode must not release replication until Fuss has applied the cut.
	// The applied file is that evidence; a sleep is not.
	if err := waitIsolateApplied(dir, "node_0", 8*time.Second); err != nil {
		return 0
	}
	if mode == "bug" && gate != "" {
		_ = os.WriteFile(gate, []byte("GO\n"), 0o644)
	}
	return 0
}

type isolateApplied struct {
	Target       string `json:"target"`
	Direction    string `json:"direction"`
	LinksCut     int    `json:"links_cut"`
	LinksTotal   int    `json:"links_total"`
	TriggerFired bool   `json:"trigger_fired"`
	CutTime      string `json:"cut_time,omitempty"`
}

func waitIsolateApplied(dir, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if doc, err := readIsolateApplied(dir); err == nil && appliedOK(doc, target) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cut not applied")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readIsolateApplied(dir string) (isolateApplied, error) {
	b, err := os.ReadFile(filepath.Join(dir, "isolate_applied.json"))
	if err != nil {
		return isolateApplied{}, err
	}
	var doc isolateApplied
	if err := json.Unmarshal(b, &doc); err != nil {
		return isolateApplied{}, err
	}
	return doc, nil
}

func appliedOK(doc isolateApplied, target string) bool {
	return doc.TriggerFired && doc.LinksCut > 0 && doc.Target == target
}

// runVerifyHistory records the post-cut read, then asks Porcupine whether
// that client history is linearizable. The cut is not an input to that
// answer. Fuss reports the experiment UNKNOWN when it cannot verify the cut,
// even if this line says the history is legal.
func runVerifyHistory(dir string, n int, key, initial, schedule string) int {
	report := func(prefix string) int {
		saved := writeSavedHistory(dir)
		if schedule != "" {
			fmt.Print(scheduleCheck(saved, initial, schedule))
			return 0
		}
		if prefix != "" {
			fmt.Printf("%stranslation effect=optional bound=open\n%s", prefix, saved)
			return 0
		}
		fmt.Print(formatHistoryCheck(saved, initial))
		return 0
	}
	rec := openRecorder(filepath.Join(dir, "client.jsonl"))
	clients := make([]string, n)
	for i := 0; i < n; i++ {
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("node%d", i), "client"))
		if err != nil {
			return report("history unknown reason=no_client\n")
		}
		clients[i] = strings.TrimSpace(string(b))
	}
	deadline := time.Now().Add(3 * time.Second)
	newLead := -1
	for time.Now().Before(deadline) && newLead < 0 {
		for i := 1; i < n; i++ {
			id, err := leaderID(clients[i])
			if err == nil && id == i {
				newLead = i
				break
			}
		}
		if newLead < 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if newLead < 1 {
		return report("history unknown reason=no_read\n")
	}
	httpClient := &http.Client{Timeout: 2 * time.Second}
	from := fmt.Sprintf("node_%d", newLead)
	rec.start("r1", "read", key, "", from)
	resp, err := httpClient.Get("http://" + clients[newLead] + "/k/" + key)
	var status int
	var body string
	if err == nil {
		status = resp.StatusCode
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			err = rerr
		} else {
			body = string(b)
		}
	}
	obs := observeHTTP(err, status)
	readValue := ""
	missing := false
	if obs == "ok" && status == http.StatusOK {
		readValue = strings.TrimSpace(body)
	}
	if obs == "ok" && status == http.StatusNotFound {
		missing = true
	}
	rec.end("r1", "read", obs, readValue, errText(err), missing)
	if schedule == "" && obs != "ok" {
		return report(fmt.Sprintf("history unknown reason=read_%s\n", obs))
	}
	return report("")
}

func runVerify(dir string, n int, key, cutNode, readFault string, porcupineOn bool, initial, schedule string) int {
	if porcupineOn {
		return runVerifyHistory(dir, n, key, initial, schedule)
	}
	defer writeSavedHistory(dir)
	if cutNode == "" {
		cutNode = "node_0"
	}
	doc, err := readIsolateApplied(dir)
	if err != nil || !appliedOK(doc, cutNode) {
		fmt.Println("UNKNOWN reason=cut_not_applied")
		return 0
	}
	clients := make([]string, n)
	for i := 0; i < n; i++ {
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("node%d", i), "client"))
		if err != nil {
			fmt.Println("UNKNOWN reason=no_client")
			return 0
		}
		clients[i] = strings.TrimSpace(string(b))
	}
	// The surviving leader is a node other than the isolated one that reports itself.
	deadline := time.Now().Add(3 * time.Second)
	newLead := -1
	for time.Now().Before(deadline) && newLead < 0 {
		for i := 1; i < n; i++ {
			id, err := leaderID(clients[i])
			if err == nil && id == i {
				newLead = i
				break
			}
		}
		if newLead < 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if newLead < 1 {
		fmt.Println("UNKNOWN reason=no_new_leader")
		return 0
	}
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + clients[newLead] + "/k/" + key
	from := fmt.Sprintf("node_%d", newLead)
	if readFault == "refused" {
		url = "http://127.0.0.1:1/k/" + key
		from = "node_refused"
	}
	rec := openRecorder()
	rec.start("r1", "read", key, "", from)
	resp, err := client.Get(url)
	var status int
	var body string
	if err == nil {
		status = resp.StatusCode
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			err = rerr
		} else {
			body = string(b)
		}
	}
	obs := observeHTTP(err, status)
	readValue := ""
	missing := false
	if obs == "ok" && status == http.StatusOK {
		readValue = strings.TrimSpace(body)
	}
	if obs == "ok" && status == http.StatusNotFound {
		missing = true
	}
	rec.end("r1", "read", obs, readValue, errText(err), missing)
	kind, reason := classifyRead(status, body, err, "v")
	fmt.Println(verifyLine(kind, reason, newLead, key))
	return 0
}

func leaderID(client string) (int, error) {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + client + "/leader")
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return -1, err
	}
	if resp.StatusCode != http.StatusOK {
		return -1, fmt.Errorf("status %d", resp.StatusCode)
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// classifyRead distinguishes a missing value from a failed query.
// kind is pass, lost, or unknown. A non-nil err, or any status other than
// 200 or 404, is unknown with reason read_error.
func classifyRead(status int, body string, err error, want string) (kind, reason string) {
	if err != nil {
		return "unknown", "read_error"
	}
	switch status {
	case http.StatusNotFound:
		return "lost", ""
	case http.StatusOK:
		if strings.TrimSpace(body) != want {
			return "unknown", "read_unexpected"
		}
		return "pass", ""
	default:
		return "unknown", "read_error"
	}
}

func verifyLine(kind, reason string, node int, key string) string {
	switch kind {
	case "pass":
		return fmt.Sprintf("PASS read_from=node_%d leader=self after_cut=yes key=%s", node, key)
	case "lost":
		return fmt.Sprintf("LOST WRITE read_from=node_%d leader=self after_cut=yes value_missing=yes key=%s", node, key)
	default:
		if reason == "" {
			reason = "unspecified"
		}
		return "UNKNOWN reason=" + reason
	}
}
