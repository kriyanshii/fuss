package proxy

import (
	"fmt"
	"math/rand"
	"net"
	"time"
)

// LinkMode is the fault applied to one direction of a Link.
type LinkMode string

const (
	ModeHealthy LinkMode = "healthy"
	ModeReset   LinkMode = "reset" // Cut: close active conns, refuse new ones
	// ModeBlackhole keeps the TCP connection open and discards payload bytes
	// while the mode is active. The proxy reads them, so the sender's Write
	// returns; the peer blocks until its own read deadline. This is not TCP
	// send-buffer backpressure. A connection that discarded bytes is closed
	// when Apply leaves ModeBlackhole on that direction, so Heal cannot
	// resume a spliced stream. Peers dial again. A pause/throttle mode would
	// be required before Write itself stalls.
	ModeBlackhole LinkMode = "blackhole"
	// ModeDegraded applies one-way latency, jitter, and DropRate. DropRate
	// discards an already-read chunk and forwards the bytes that follow, so
	// framing (HTTP bodies, length-prefixed Raft, gRPC) desynchronizes while
	// the socket stays up. New dials are still accepted.
	ModeDegraded LinkMode = "degraded"
)

const (
	// DirBoth applies a profile to client-to-upstream and upstream-to-client.
	DirBoth = "both"
	// DirAToB is the accepted client toward the upstream.
	DirAToB = "a_to_b"
	// DirBToA is the upstream toward the accepted client.
	DirBToA = "b_to_a"
)

// FaultProfile is one link's fault. Latency and Jitter are one-way.
// Direction "a_to_b" is client to upstream; "b_to_a" is the reverse.
// DropRate is the probability of discarding one already-read TCP chunk.
// The socket stays open and later chunks are still forwarded, which breaks
// message framing. DropRate does not refuse a new dial and does not stall
// the sender. Jitter and DropRate draw on the *rand.Rand passed to Apply.
// A nil generator applies Latency with no jitter and drops only when
// DropRate >= 1.
type FaultProfile struct {
	Mode      LinkMode
	Latency   time.Duration
	Jitter    time.Duration
	DropRate  float64
	Direction string
}

// relayPoll is how often an idle relay re-reads the fault profile.
const relayPoll = 100 * time.Millisecond

// Apply sets the link fault. A nil rng keeps the generator from the previous
// call. ModeReset with direction both closes open connections and refuses new
// ones, matching Cut. Leaving ModeBlackhole closes connections that discarded
// bytes on that direction. Other mode changes leave clean sockets open.
func (l *Link) Apply(p FaultProfile, rng *rand.Rand) error {
	p, err := normalizeFault(p)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if rng != nil {
		l.rng = rng
	}
	if p.Mode == ModeHealthy && p.Direction == DirBoth {
		l.cut = false
		l.refuse = nil
		l.overlay = nil
		l.toA = FaultProfile{Mode: ModeHealthy, Direction: DirAToB}
		l.toB = FaultProfile{Mode: ModeHealthy, Direction: DirBToA}
		l.lastHeal.Store(time.Now())
		l.notifyDirLocked(DirBoth)
		conns, tags := l.takeDirtyLocked()
		l.mu.Unlock()
		l.closeTracked(conns, tags)
		return nil
	}
	l.setDirLocked(p)
	bothReset := l.toA.Mode == ModeReset && l.toB.Mode == ModeReset
	if bothReset {
		l.cut = true
		l.lastCut.Store(time.Now())
		l.notifyDirLocked(DirBoth)
		conns, tags := takeAll(l)
		l.mu.Unlock()
		l.closeTracked(conns, tags)
		return nil
	}
	l.cut = false
	if p.Mode != ModeHealthy {
		l.lastCut.Store(time.Now())
	} else if l.toA.Mode == ModeHealthy && l.toB.Mode == ModeHealthy {
		l.refuse = nil
		l.lastHeal.Store(time.Now())
	}
	l.notifyDirLocked(p.Direction)
	conns, tags := l.takeDirtyLocked()
	l.mu.Unlock()
	l.closeTracked(conns, tags)
	return nil
}

// ApplyWhere faults only connections whose tag matches. Other sources on the
// same shared inbound proxy keep the link profile. A later Apply of healthy
// on both directions clears it. The match runs under the link lock.
func (l *Link) ApplyWhere(match func(tag int, known bool) bool, p FaultProfile) error {
	if match == nil {
		return nil
	}
	p, err := normalizeFault(p)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.overlay = &tagOverlay{match: match, profile: p}
	if p.Mode != ModeHealthy {
		l.lastCut.Store(time.Now())
	}
	l.notifyDirLocked(p.Direction)
	conns, tags := l.takeDirtyLocked()
	l.mu.Unlock()
	l.closeTracked(conns, tags)
	return nil
}

// closeTracked counts and closes client sockets removed from the link.
func (l *Link) closeTracked(conns []net.Conn, tags []int) {
	l.noteClosed(len(conns), tags)
	for _, c := range conns {
		_ = c.Close()
	}
}

// takeDirtyLocked removes connections that discarded bytes on a direction
// which is no longer ModeBlackhole. Caller holds l.mu.
func (l *Link) takeDirtyLocked() ([]net.Conn, []int) {
	var conns []net.Conn
	var tags []int
	for c, info := range l.conns {
		if !l.dirtyShouldClose(info) {
			continue
		}
		conns = append(conns, c)
		delete(l.conns, c)
		if info.known {
			tags = append(tags, info.tag)
		}
	}
	return conns, tags
}

func (l *Link) dirtyShouldClose(info connMeta) bool {
	if info.dirtyA && !l.dirBlackholeFor(info, 0) {
		return true
	}
	if info.dirtyB && !l.dirBlackholeFor(info, 1) {
		return true
	}
	return false
}

// dirBlackholeFor reports whether this connection still discards on dir.
// dir 0 is client to upstream. dir 1 is upstream to client.
func (l *Link) dirBlackholeFor(info connMeta, dir int) bool {
	if l.overlay != nil && l.overlay.profile.Mode == ModeBlackhole &&
		l.overlay.match(info.tag, info.known) &&
		overlayApplies(l.overlay.profile.Direction, dir) {
		return true
	}
	return l.profileLocked(dir).Mode == ModeBlackhole
}

func (l *Link) markDirtyLocked(client net.Conn, dir int) {
	info, ok := l.conns[client]
	if !ok {
		return
	}
	if dir == 0 {
		info.dirtyA = true
	} else {
		info.dirtyB = true
	}
	l.conns[client] = info
}

func (l *Link) resolveLocked(client net.Conn, dir int) FaultProfile {
	p := l.profileLocked(dir)
	if info, ok := l.conns[client]; ok && l.overlay != nil && l.overlay.match(info.tag, info.known) {
		if overlayApplies(l.overlay.profile.Direction, dir) {
			p = l.overlay.profile
		}
	}
	return p
}

// Profiles returns the client-to-upstream profile and the upstream-to-client profile.
func (l *Link) Profiles() (aToB, bToA FaultProfile) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.toA, l.toB
}

func normalizeFault(p FaultProfile) (FaultProfile, error) {
	switch p.Mode {
	case "":
		p.Mode = ModeHealthy
	case ModeHealthy, ModeReset, ModeBlackhole, ModeDegraded:
	default:
		return FaultProfile{}, fmt.Errorf("proxy: unknown link mode %q", p.Mode)
	}
	switch p.Direction {
	case "":
		p.Direction = DirBoth
	case DirBoth, DirAToB, DirBToA:
	default:
		return FaultProfile{}, fmt.Errorf("proxy: unknown link direction %q", p.Direction)
	}
	if p.Latency < 0 || p.Jitter < 0 {
		return FaultProfile{}, fmt.Errorf("proxy: negative delay")
	}
	if p.DropRate < 0 || p.DropRate > 1 {
		return FaultProfile{}, fmt.Errorf("proxy: drop rate %v not in [0,1]", p.DropRate)
	}
	return p, nil
}

func (l *Link) setDirLocked(p FaultProfile) {
	switch p.Direction {
	case DirAToB:
		p.Direction = DirAToB
		l.toA = p
	case DirBToA:
		p.Direction = DirBToA
		l.toB = p
	default:
		a := p
		a.Direction = DirAToB
		b := p
		b.Direction = DirBToA
		l.toA = a
		l.toB = b
	}
}

func (l *Link) notifyDirLocked(direction string) {
	switch direction {
	case DirAToB:
		l.bumpWake(0)
	case DirBToA:
		l.bumpWake(1)
	default:
		l.bumpWake(0)
		l.bumpWake(1)
	}
}

func (l *Link) bumpWake(i int) {
	if l.wake[i] != nil {
		close(l.wake[i])
	}
	l.wake[i] = make(chan struct{})
}

func overlayApplies(direction string, dir int) bool {
	switch direction {
	case DirAToB:
		return dir == 0
	case DirBToA:
		return dir == 1
	default:
		return true
	}
}

func (l *Link) profileLocked(dir int) FaultProfile {
	if dir == 0 {
		return l.toA
	}
	return l.toB
}

// relay copies one direction until the socket closes or both directions are reset.
// Every chunk reads the live FaultProfile under l.mu. Apply on an existing
// stream takes effect on the next chunk. A delay already in progress is woken
// and that chunk is classified again before it is written.
func (l *Link) relay(dst, src net.Conn, dir int) {
	buf := make([]byte, 32*1024)
	for {
		if !l.relayOpen() {
			return
		}
		_ = src.SetReadDeadline(time.Now().Add(relayPoll))
		n, err := src.Read(buf)
		if n > 0 && !l.forward(dst, src, buf[:n], dir) {
			return
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
	}
}

func (l *Link) relayOpen() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	return !(l.toA.Mode == ModeReset && l.toB.Mode == ModeReset)
}

// forward delivers one chunk. false means the relay should stop.
func (l *Link) forward(dst, src net.Conn, payload []byte, dir int) bool {
	client := src
	if dir == 1 {
		client = dst
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return false
	}
	if _, tracked := l.conns[client]; !tracked {
		// Apply already closed this socket. Do not write a chunk that was
		// read before the close onto the surviving half of the stream.
		l.mu.Unlock()
		return false
	}
	p := l.resolveLocked(client, dir)
	fullReset := l.toA.Mode == ModeReset && l.toB.Mode == ModeReset
	var delay time.Duration
	drop := false
	switch p.Mode {
	case ModeReset:
		l.mu.Unlock()
		return !fullReset
	case ModeBlackhole:
		l.markDirtyLocked(client, dir)
		l.mu.Unlock()
		return true
	case ModeDegraded:
		drop = randDrop(l.rng, p.DropRate)
		if !drop {
			delay = randDelay(l.rng, p.Latency, p.Jitter)
		}
	}
	l.mu.Unlock()
	if drop {
		return true
	}
	if delay > 0 {
		l.sleepOrWake(dir, delay)
		l.mu.Lock()
		closed := l.closed
		_, tracked := l.conns[client]
		p = l.resolveLocked(client, dir)
		fullReset = l.toA.Mode == ModeReset && l.toB.Mode == ModeReset
		if tracked && !closed && !fullReset && p.Mode == ModeBlackhole {
			l.markDirtyLocked(client, dir)
		}
		l.mu.Unlock()
		if closed || fullReset || !tracked {
			return false
		}
		if p.Mode == ModeBlackhole || p.Mode == ModeReset {
			return true
		}
	}
	_, err := dst.Write(payload)
	return err == nil
}

func (l *Link) sleepOrWake(dir int, d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	ch := l.wake[dir]
	l.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ch:
	}
}

func randDelay(rng *rand.Rand, latency, jitter time.Duration) time.Duration {
	if latency < 0 {
		latency = 0
	}
	if jitter <= 0 || rng == nil {
		return latency
	}
	span := int64(jitter)*2 + 1
	if span <= 0 {
		return latency
	}
	d := latency + time.Duration(rng.Int63n(span)-int64(jitter))
	if d < 0 {
		return 0
	}
	return d
}

func randDrop(rng *rand.Rand, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	if rng == nil {
		return false
	}
	return rng.Float64() < rate
}
