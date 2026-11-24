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
	conns  map[net.Conn]struct{} // client-side connections only

	accepted        atomic.Int64
	closedByCut     atomic.Int64
	refusedWhileCut atomic.Int64
	lastCut         atomic.Value // time.Time
	lastHeal        atomic.Value // time.Time

	wg sync.WaitGroup
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
		conns:    make(map[net.Conn]struct{}),
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
	conns := takeConns(l)
	l.mu.Unlock()

	if n := int64(len(conns)); n > 0 {
		l.closedByCut.Add(n)
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// Heal resumes accepting and forwarding after a Cut.
func (l *Link) Heal() {
	l.mu.Lock()
	l.cut = false
	l.lastHeal.Store(time.Now())
	l.mu.Unlock()
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

// takeConns drains the client connection set. Caller must hold l.mu.
func takeConns(l *Link) []net.Conn {
	conns := make([]net.Conn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = make(map[net.Conn]struct{})
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

		l.mu.Lock()
		if l.cut || l.closed {
			l.mu.Unlock()
			l.refusedWhileCut.Add(1)
			_ = c.Close()
			continue
		}
		l.conns[c] = struct{}{}
		l.mu.Unlock()

		l.wg.Add(1)
		go l.handle(c)
	}
}

func (l *Link) handle(client net.Conn) {
	defer l.wg.Done()
	defer l.removeConn(client)
	defer client.Close()

	up, err := net.Dial("tcp", l.upstream)
	if err != nil {
		return
	}
	defer up.Close()

	// If Cut landed while dialing, drop without forwarding.
	l.mu.Lock()
	drop := l.cut || l.closed
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
