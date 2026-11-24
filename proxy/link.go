// Package proxy is a spike TCP forwarder with Cut/Heal for fault-injection experiments.
package proxy

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Stats is a snapshot of Link counters.
type Stats struct {
	Open            int
	Accepted        int64
	ClosedByCut     int64
	RefusedWhileCut int64
	LastCut         time.Time
	LastHeal        time.Time
}

// Link listens on a local address and forwards TCP to one upstream, both ways.
type Link struct {
	listen   string
	upstream string

	ln net.Listener

	mu     sync.Mutex
	cut    bool
	closed bool
	conns  map[net.Conn]connMeta // client-side connections only
	tagger func(net.Addr) (tag int, known bool)
	// refuse reports connections to reject after a partial cut. Nil refuses none.
	refuse func(tag int, known bool) bool

	accepted        atomic.Int64
	closedByCut     atomic.Int64
	refusedWhileCut atomic.Int64
	lastCut         atomic.Value // time.Time
	lastHeal        atomic.Value // time.Time

	srcMu sync.Mutex
	byTag map[int]*tagStats // per source tag; unknown owners are not entered

	wg sync.WaitGroup
}

// connMeta is the label on one accepted client connection.
type connMeta struct {
	tag   int
	known bool
}

// tagStats counts one source tag. A shared inbound proxy has one Link and
// several tags; evidence for a kept source must not reuse another tag's totals.
type tagStats struct {
	accepted        atomic.Int64
	closedByCut     atomic.Int64
	refusedWhileCut atomic.Int64
}

// New prepares a Link. Call Start to begin accepting.
func New(listen, upstream string) (*Link, error) {
	if listen == "" {
		return nil, fmt.Errorf("proxy: empty listen address")
	}
	if upstream == "" {
		return nil, fmt.Errorf("proxy: empty upstream address")
	}
	l := &Link{
		listen:   listen,
		upstream: upstream,
		conns:    make(map[net.Conn]connMeta),
	}
	l.lastCut.Store(time.Time{})
	l.lastHeal.Store(time.Time{})
	return l, nil
}

// Start begins listening and accepting connections.
func (l *Link) Start() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("proxy: link closed")
	}
	if l.ln != nil {
		return fmt.Errorf("proxy: already started")
	}
	ln, err := net.Listen("tcp", l.listen)
	if err != nil {
		return err
	}
	l.ln = ln
	l.wg.Add(1)
	go l.acceptLoop()
	return nil
}

// Addr returns the actual listen address (useful when listen was ":0").
func (l *Link) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return nil
	}
	return l.ln.Addr()
}

// Close stops accepting and closes every open connection.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	ln := l.ln
	l.ln = nil
	conns := takeConns(l)
	l.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	l.wg.Wait()
	return nil
}

// Cut closes every open connection immediately and refuses new ones until Heal.
func (l *Link) Cut() {
	l.mu.Lock()
	l.cut = true
	l.lastCut.Store(time.Now())
	conns, tags := takeAll(l)
	l.mu.Unlock()

	l.noteClosed(len(conns), tags)
	for _, c := range conns {
		_ = c.Close()
	}
}

// Heal resumes accepting and forwarding after a Cut or CutWhere.
func (l *Link) Heal() {
	l.mu.Lock()
	l.cut = false
	l.refuse = nil
	l.lastHeal.Store(time.Now())
	l.mu.Unlock()
}

// SetTagger labels each accepted connection before it is forwarded.
// known is false when the owner cannot be determined; the connection is still kept.
// A nil tagger leaves connections unlabeled.
func (l *Link) SetTagger(fn func(remote net.Addr) (tag int, known bool)) {
	l.mu.Lock()
	l.tagger = fn
	l.mu.Unlock()
}

// CutWhere closes open connections for which match is true and refuses later
// accepts that match. Connections that do not match stay open.
func (l *Link) CutWhere(match func(tag int, known bool) bool) {
	if match == nil {
		return
	}
	l.mu.Lock()
	var closeNow []net.Conn
	var closedTags []int
	for c, info := range l.conns {
		if match(info.tag, info.known) {
			closeNow = append(closeNow, c)
			delete(l.conns, c)
			if info.known {
				closedTags = append(closedTags, info.tag)
			}
		}
	}
	prev := l.refuse
	l.refuse = func(tag int, known bool) bool {
		if prev != nil && prev(tag, known) {
			return true
		}
		return match(tag, known)
	}
	l.lastCut.Store(time.Now())
	l.mu.Unlock()

	l.noteClosed(len(closeNow), closedTags)
	for _, c := range closeNow {
		_ = c.Close()
	}
}

// TagStats is the counters for one source tag. Unknown owners are omitted.
func (l *Link) TagStats(tag int) Stats {
	l.srcMu.Lock()
	b := l.byTag[tag]
	var st Stats
	if b != nil {
		st.Accepted = b.accepted.Load()
		st.ClosedByCut = b.closedByCut.Load()
		st.RefusedWhileCut = b.refusedWhileCut.Load()
	}
	l.srcMu.Unlock()
	l.mu.Lock()
	for _, info := range l.conns {
		if info.known && info.tag == tag {
			st.Open++
		}
	}
	l.mu.Unlock()
	return st
}

// AllTagStats snapshots every source tag. Open is the connections still tracked.
func (l *Link) AllTagStats() map[int]Stats {
	l.mu.Lock()
	open := map[int]int{}
	for _, info := range l.conns {
		if info.known {
			open[info.tag]++
		}
	}
	l.mu.Unlock()

	l.srcMu.Lock()
	defer l.srcMu.Unlock()
	out := make(map[int]Stats, len(l.byTag))
	for tag, b := range l.byTag {
		st := Stats{
			Accepted:        b.accepted.Load(),
			ClosedByCut:     b.closedByCut.Load(),
			RefusedWhileCut: b.refusedWhileCut.Load(),
			Open:            open[tag],
		}
		out[tag] = st
	}
	return out
}

// Stats returns a snapshot of counters.
func (l *Link) Stats() Stats {
	l.mu.Lock()
	open := len(l.conns)
	l.mu.Unlock()
	lastCut, _ := l.lastCut.Load().(time.Time)
	lastHeal, _ := l.lastHeal.Load().(time.Time)
	return Stats{
		Open:            open,
		Accepted:        l.accepted.Load(),
		ClosedByCut:     l.closedByCut.Load(),
		RefusedWhileCut: l.refusedWhileCut.Load(),
		LastCut:         lastCut,
		LastHeal:        lastHeal,
	}
}

// noteTagged counts one attributed connection. Refused dials also count as accepted,
// matching the listener-level accepted counter.
func (l *Link) noteTagged(tag int, known, refused bool) {
	if !known {
		return
	}
	b := l.tagBucket(tag)
	b.accepted.Add(1)
	if refused {
		b.refusedWhileCut.Add(1)
	}
}

func (l *Link) noteClosed(n int, tags []int) {
	if n > 0 {
		l.closedByCut.Add(int64(n))
	}
	for _, tag := range tags {
		l.tagBucket(tag).closedByCut.Add(1)
	}
}

func (l *Link) tagBucket(tag int) *tagStats {
	l.srcMu.Lock()
	defer l.srcMu.Unlock()
	if l.byTag == nil {
		l.byTag = make(map[int]*tagStats)
	}
	b := l.byTag[tag]
	if b == nil {
		b = &tagStats{}
		l.byTag[tag] = b
	}
	return b
}

// takeAll drains the client connection set and the known source tags in it.
// Caller must hold l.mu. ClosedByCut is not updated here.
func takeAll(l *Link) ([]net.Conn, []int) {
	conns := make([]net.Conn, 0, len(l.conns))
	var tags []int
	for c, info := range l.conns {
		conns = append(conns, c)
		if info.known {
			tags = append(tags, info.tag)
		}
	}
	l.conns = make(map[net.Conn]connMeta)
	return conns, tags
}

// takeConns drains the client connection set. Caller must hold l.mu.
func takeConns(l *Link) []net.Conn {
	conns := make([]net.Conn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = make(map[net.Conn]connMeta)
	return conns
}

func (l *Link) acceptLoop() {
	defer l.wg.Done()
	for {
		l.mu.Lock()
		ln := l.ln
		l.mu.Unlock()
		if ln == nil {
			return
		}
		c, err := ln.Accept()
		if err != nil {
			l.mu.Lock()
			done := l.closed || l.ln == nil
			l.mu.Unlock()
			if done {
				return
			}
			continue
		}
		l.accepted.Add(1)
		l.wg.Add(1)
		go l.admit(c)
	}
}

func (l *Link) admit(c net.Conn) {
	defer l.wg.Done()

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = c.Close()
		return
	}
	// Tag even on a full cut so a refused dial is counted for its source.
	// Unknown owners stay out of the per-source counters.
	tagger := l.tagger
	l.mu.Unlock()

	meta := connMeta{}
	if tagger != nil {
		tag, known := tagger(c.RemoteAddr())
		meta = connMeta{tag: tag, known: known}
	}

	l.mu.Lock()
	refuse := l.closed || l.cut || (l.refuse != nil && l.refuse(meta.tag, meta.known))
	if refuse {
		l.mu.Unlock()
		l.noteTagged(meta.tag, meta.known, true)
		l.refusedWhileCut.Add(1)
		_ = c.Close()
		return
	}
	l.noteTagged(meta.tag, meta.known, false)
	l.conns[c] = meta
	l.mu.Unlock()

	l.handle(c)
}

func (l *Link) handle(client net.Conn) {
	defer l.removeConn(client)
	defer client.Close()

	up, err := net.Dial("tcp", l.upstream)
	if err != nil {
		return
	}
	defer up.Close()

	// If Cut or CutWhere landed while dialing, drop without forwarding.
	l.mu.Lock()
	_, tracked := l.conns[client]
	drop := l.cut || l.closed || !tracked
	l.mu.Unlock()
	if drop {
		return
	}

	errc := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, client)
		errc <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, up)
		errc <- struct{}{}
	}()
	<-errc
	_ = client.Close()
	_ = up.Close()
	<-errc
}

func (l *Link) removeConn(c net.Conn) {
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}
