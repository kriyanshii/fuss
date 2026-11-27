package proxy_test

import (
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"fuss/proxy"
)

func startEcho(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}

func startLink(t *testing.T, upstream string) *proxy.Link {
	t.Helper()
	link, err := proxy.New("127.0.0.1:0", upstream)
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = link.Close() })
	return link
}

func echoRoundTrip(t *testing.T, addr string, payload []byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q want %q", got, payload)
	}
}

func TestEchoRoundTrip(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	echoRoundTrip(t, link.Addr().String(), []byte("hello-proxy"))
}

func TestCutClosesOpenConnection(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Wait until the link has registered the connection.
	deadline := time.Now().Add(time.Second)
	for link.Stats().Open < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for open connection")
		}
		time.Sleep(5 * time.Millisecond)
	}

	before := link.Stats().ClosedByCut
	link.Cut()

	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, err = c.Read(buf)
	if err == nil {
		t.Fatal("expected EOF or reset after Cut within 1s, got nil")
	}

	st := link.Stats()
	if st.ClosedByCut != before+1 {
		t.Fatalf("ClosedByCut=%d want %d", st.ClosedByCut, before+1)
	}
	if st.LastCut.IsZero() {
		t.Fatal("LastCut not set")
	}
	if st.Open != 0 {
		t.Fatalf("Open=%d want 0", st.Open)
	}
}

func TestRefuseWhileCutThenHeal(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	link.Cut()
	before := link.Stats().RefusedWhileCut

	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		// Dial failed outright — also acceptable while cut.
		st := link.Stats()
		if st.RefusedWhileCut < before+1 {
			// Accept may not have run yet if dial failed at OS level; retry briefly.
			deadline := time.Now().Add(time.Second)
			for link.Stats().RefusedWhileCut < before+1 && time.Now().Before(deadline) {
				_, _ = net.DialTimeout("tcp", link.Addr().String(), 50*time.Millisecond)
				time.Sleep(10 * time.Millisecond)
			}
		}
		st = link.Stats()
		if st.RefusedWhileCut < before+1 {
			t.Fatalf("RefusedWhileCut=%d want >= %d (dial err %v)", st.RefusedWhileCut, before+1, err)
		}
	} else {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1)
		_, readErr := c.Read(buf)
		_ = c.Close()
		if readErr == nil {
			t.Fatal("expected immediate close while cut")
		}
		deadline := time.Now().Add(time.Second)
		for link.Stats().RefusedWhileCut < before+1 {
			if time.Now().After(deadline) {
				t.Fatalf("RefusedWhileCut=%d want >= %d", link.Stats().RefusedWhileCut, before+1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	link.Heal()
	if link.Stats().LastHeal.IsZero() {
		t.Fatal("LastHeal not set")
	}

	echoRoundTrip(t, link.Addr().String(), []byte("after-heal"))
}

func TestCutWhereKeepsOtherTag(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	var mu sync.Mutex
	owners := map[string]int{}
	link.SetTagger(func(a net.Addr) (int, bool) {
		if a == nil {
			return 0, false
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			tag, ok := owners[a.String()]
			mu.Unlock()
			if ok {
				if tag < 0 {
					return 0, false
				}
				return tag, true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0, false
	})

	dialTagged := func(tag int) net.Conn {
		t.Helper()
		c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		owners[c.LocalAddr().String()] = tag
		mu.Unlock()
		return c
	}
	waitOpen := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for link.Stats().Open < n {
			if time.Now().After(deadline) {
				t.Fatalf("open=%d want >= %d", link.Stats().Open, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	c0 := dialTagged(0)
	defer c0.Close()
	c1 := dialTagged(1)
	defer c1.Close()
	unknown := dialTagged(-1)
	defer unknown.Close()
	waitOpen(3)

	link.CutWhere(func(tag int, known bool) bool { return known && tag == 0 })

	buf := make([]byte, 1)
	_ = c0.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c0.Read(buf); err == nil {
		t.Fatal("tag 0 connection still open after CutWhere")
	}
	if _, err := c1.Write([]byte("ping")); err != nil {
		t.Fatalf("kept tag write: %v", err)
	}
	got := make([]byte, 4)
	_ = c1.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c1, got); err != nil {
		t.Fatalf("kept tag read: %v", err)
	}
	if _, err := unknown.Write([]byte("ping")); err != nil {
		t.Fatalf("unknown write: %v", err)
	}
	if _, err := io.ReadFull(unknown, got); err != nil {
		t.Fatalf("unknown read: %v", err)
	}

	refused := dialTagged(0)
	defer refused.Close()
	_ = refused.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := refused.Read(buf); err == nil {
		t.Fatal("new tag 0 connection was not refused")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		s0 := link.TagStats(0)
		s1 := link.TagStats(1)
		if s0.ClosedByCut >= 1 && s0.RefusedWhileCut >= 1 && s1.ClosedByCut == 0 && s1.RefusedWhileCut == 0 && s1.Accepted >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tag stats tag0=%+v tag1=%+v", s0, s1)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestApplyWhereBlackholesOneTag(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	var mu sync.Mutex
	owners := map[string]int{}
	link.SetTagger(func(a net.Addr) (int, bool) {
		if a == nil {
			return 0, false
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			tag, ok := owners[a.String()]
			mu.Unlock()
			if ok {
				return tag, true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0, false
	})
	dialTagged := func(tag int) net.Conn {
		t.Helper()
		c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		owners[c.LocalAddr().String()] = tag
		mu.Unlock()
		return c
	}
	c0 := dialTagged(0)
	defer c0.Close()
	c1 := dialTagged(1)
	defer c1.Close()
	waitOpen(t, link, 2)
	if _, err := c0.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	_ = c0.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c0, got); err != nil {
		t.Fatal(err)
	}

	if err := link.ApplyWhere(func(tag int, known bool) bool {
		return known && tag == 0
	}, proxy.FaultProfile{Mode: proxy.ModeBlackhole, Direction: proxy.DirBoth}); err != nil {
		t.Fatal(err)
	}
	if _, err := c0.Write([]byte("drop")); err != nil {
		t.Fatalf("blackhole write: %v", err)
	}
	wantReadTimeout(t, c0, 300*time.Millisecond)
	if _, err := c1.Write([]byte("keep")); err != nil {
		t.Fatalf("other tag write: %v", err)
	}
	_ = c1.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c1, got); err != nil {
		t.Fatalf("other tag read: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("other tag got %q", got)
	}

	link.Heal()
	wantClosed(t, c0)
	if _, err := c1.Write([]byte("same")); err != nil {
		t.Fatalf("untouched tag write: %v", err)
	}
	_ = c1.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c1, got); err != nil {
		t.Fatalf("untouched tag read: %v", err)
	}
	if string(got) != "same" {
		t.Fatalf("untouched tag got %q", got)
	}
	fresh := dialTagged(0)
	defer fresh.Close()
	if _, err := fresh.Write([]byte("new0")); err != nil {
		t.Fatalf("new tag 0 write: %v", err)
	}
	_ = fresh.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(fresh, got); err != nil {
		t.Fatalf("new tag 0 read: %v", err)
	}
	if string(got) != "new0" {
		t.Fatalf("new tag 0 got %q", got)
	}
}

func waitOpen(t *testing.T, link *proxy.Link, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for link.Stats().Open < n {
		if time.Now().After(deadline) {
			t.Fatalf("open=%d want >= %d", link.Stats().Open, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantReadTimeout(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	start := time.Now()
	buf := make([]byte, 8)
	_, err := c.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout, got data")
	}
	ne, ok := err.(net.Error)
	if !ok || !ne.Timeout() {
		t.Fatalf("got %v after %s, want a deadline timeout", err, elapsed)
	}
	if elapsed < d*3/4 {
		t.Fatalf("read returned in %s, want to block until about %s", elapsed, d)
	}
}

// wantClosed fails when c still returns a payload after Heal. A timeout means
// the socket stayed open. A successful read means post-heal bytes were spliced
// onto the stream that discarded earlier bytes.
func wantClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("post-heal"))
	buf := make([]byte, 16)
	_, err := c.Read(buf)
	if err == nil {
		t.Fatalf("dirty connection returned %q after heal", buf)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("dirty connection stalled after heal, want close")
	}
}

func TestApplyResetClosesAndRefuses(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitOpen(t, link, 1)

	before := link.Stats().ClosedByCut
	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeReset, Direction: proxy.DirBoth}, nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected EOF or reset after ModeReset")
	}
	if link.Stats().ClosedByCut != before+1 {
		t.Fatalf("ClosedByCut=%d want %d", link.Stats().ClosedByCut, before+1)
	}

	refusedBefore := link.Stats().RefusedWhileCut
	c2, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err == nil {
		defer c2.Close()
		_ = c2.SetReadDeadline(time.Now().Add(time.Second))
		if _, rerr := c2.Read(buf); rerr == nil {
			t.Fatal("new connection was forwarded while reset")
		}
	}
	deadline := time.Now().Add(time.Second)
	for link.Stats().RefusedWhileCut < refusedBefore+1 {
		if time.Now().After(deadline) {
			t.Fatalf("RefusedWhileCut=%d want >= %d", link.Stats().RefusedWhileCut, refusedBefore+1)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBlackholeBlocksUntilDeadline(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitOpen(t, link, 1)
	echoRoundTrip(t, link.Addr().String(), []byte("before"))

	closed := link.Stats().ClosedByCut
	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeBlackhole, Direction: proxy.DirBoth}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("stalled")); err != nil {
		t.Fatalf("write during blackhole: %v", err)
	}
	wantReadTimeout(t, c, 200*time.Millisecond)
	st := link.Stats()
	if st.ClosedByCut != closed {
		t.Fatalf("ClosedByCut=%d want %d", st.ClosedByCut, closed)
	}
	if st.RefusedWhileCut != 0 {
		t.Fatalf("RefusedWhileCut=%d want 0", st.RefusedWhileCut)
	}
	if st.Open < 1 {
		t.Fatalf("Open=%d, blackhole closed the socket", st.Open)
	}

	link.Heal()
	echoRoundTrip(t, link.Addr().String(), []byte("after-heal"))
}

func connRoundTrip(t *testing.T, c net.Conn, payload []byte) time.Duration {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q want %q", got, payload)
	}
	return time.Since(start)
}

// TestMidStreamApplySameConn reads the fault profile per chunk. A blackhole
// that discarded bytes closes that socket on Heal. A connection opened after
// the heal stays up across a later degraded profile.
func TestMidStreamApplySameConn(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitOpen(t, link, 1)

	if elapsed := connRoundTrip(t, c, []byte("healthy")); elapsed > 500*time.Millisecond {
		t.Fatalf("healthy round trip took %s", elapsed)
	}

	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeBlackhole, Direction: proxy.DirBoth}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hidden")); err != nil {
		t.Fatalf("write during blackhole: %v", err)
	}
	wantReadTimeout(t, c, 200*time.Millisecond)
	if link.Stats().Open < 1 || link.Stats().ClosedByCut != 0 {
		t.Fatalf("blackhole changed the socket: %+v", link.Stats())
	}

	link.Heal()
	wantClosed(t, c)

	c2, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitOpen(t, link, 1)
	if elapsed := connRoundTrip(t, c2, []byte("healed")); elapsed > 500*time.Millisecond {
		t.Fatalf("healed round trip took %s", elapsed)
	}

	const latency = 40 * time.Millisecond
	if err := link.Apply(proxy.FaultProfile{
		Mode:      proxy.ModeDegraded,
		Direction: proxy.DirBoth,
		Latency:   latency,
	}, nil); err != nil {
		t.Fatal(err)
	}
	elapsed := connRoundTrip(t, c2, []byte("slowww"))
	if elapsed < latency*2-10*time.Millisecond {
		t.Fatalf("same-conn echo took %s, want at least %s", elapsed, latency*2)
	}
}

// TestBlackholeHealClosesDirtyConn is the blackhole contract: an open socket
// stalls during the fault, Heal closes it, and a new dial echoes.
func TestBlackholeHealClosesDirtyConn(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)

	dirty, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer dirty.Close()
	idle, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	waitOpen(t, link, 2)

	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeBlackhole, Direction: proxy.DirBoth}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dirty.Write([]byte("hidden")); err != nil {
		t.Fatalf("write during blackhole: %v", err)
	}
	wantReadTimeout(t, dirty, 200*time.Millisecond)
	if link.Stats().Open < 2 {
		t.Fatalf("blackhole closed a socket: %+v", link.Stats())
	}

	link.Heal()
	wantClosed(t, dirty)
	if elapsed := connRoundTrip(t, idle, []byte("idleok")); elapsed > 500*time.Millisecond {
		t.Fatalf("idle connection after heal took %s", elapsed)
	}
	echoRoundTrip(t, link.Addr().String(), []byte("fresh"))
}

func TestBlackholeNewConnHangs(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)
	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeBlackhole}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial during blackhole: %v", err)
	}
	defer c.Close()
	waitOpen(t, link, 1)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	wantReadTimeout(t, c, 200*time.Millisecond)
	if link.Stats().RefusedWhileCut != 0 {
		t.Fatalf("RefusedWhileCut=%d, blackhole refused the handshake", link.Stats().RefusedWhileCut)
	}
}

func TestDegradedLatencyAndDrop(t *testing.T) {
	up, closeUp := startEcho(t)
	defer closeUp()
	link := startLink(t, up)
	const latency = 40 * time.Millisecond
	if err := link.Apply(proxy.FaultProfile{
		Mode:      proxy.ModeDegraded,
		Direction: proxy.DirBoth,
		Latency:   latency,
	}, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	echoRoundTrip(t, link.Addr().String(), []byte("slow"))
	elapsed := time.Since(start)
	// One-way delay on both directions of the echo.
	if elapsed < latency*2-10*time.Millisecond {
		t.Fatalf("echo took %s, want at least %s", elapsed, latency*2)
	}
	if elapsed > time.Second {
		t.Fatalf("echo took %s, latency overshot", elapsed)
	}
	if link.Stats().ClosedByCut != 0 {
		t.Fatalf("degraded closed connections: %+v", link.Stats())
	}

	rng := rand.New(rand.NewSource(1))
	if err := link.Apply(proxy.FaultProfile{
		Mode:      proxy.ModeDegraded,
		Direction: proxy.DirBoth,
		DropRate:  1,
	}, rng); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitOpen(t, link, 1)
	if _, err := c.Write([]byte("drop-me")); err != nil {
		t.Fatal(err)
	}
	wantReadTimeout(t, c, 200*time.Millisecond)

	link.Heal()
	echoRoundTrip(t, link.Addr().String(), []byte("restored"))
}

func TestAsymmetricBlackhole(t *testing.T) {
	t.Run("a_to_b", func(t *testing.T) {
		assertDirection(t, proxy.DirAToB, "")
	})
	t.Run("b_to_a", func(t *testing.T) {
		assertDirection(t, proxy.DirBToA, "ping")
	})
}

func assertDirection(t *testing.T, direction, wantUpstream string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		buf := make([]byte, 16)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
		if n > 0 {
			_, _ = c.Write([]byte("PONG"))
		}
		// Stay open so the proxy does not see a peer close during the client timeout.
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Read(buf)
	}()

	link := startLink(t, ln.Addr().String())
	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeBlackhole, Direction: direction}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", link.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitOpen(t, link, 1)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != wantUpstream {
			t.Fatalf("upstream got %q want %q", s, wantUpstream)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not report")
	}
	wantReadTimeout(t, c, 200*time.Millisecond)
	a, b := link.Profiles()
	if direction == proxy.DirAToB && (a.Mode != proxy.ModeBlackhole || b.Mode != proxy.ModeHealthy) {
		t.Fatalf("profiles a=%+v b=%+v", a, b)
	}
	if direction == proxy.DirBToA && (b.Mode != proxy.ModeBlackhole || a.Mode != proxy.ModeHealthy) {
		t.Fatalf("profiles a=%+v b=%+v", a, b)
	}
}

func TestApplyRejectsBadProfile(t *testing.T) {
	link := startLink(t, "127.0.0.1:1")
	if err := link.Apply(proxy.FaultProfile{Mode: "nope"}, nil); err == nil {
		t.Fatal("expected unknown mode error")
	}
	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeDegraded, DropRate: 1.5}, nil); err == nil {
		t.Fatal("expected drop rate error")
	}
	if err := link.Apply(proxy.FaultProfile{Mode: proxy.ModeDegraded, Direction: "inbound"}, nil); err == nil {
		t.Fatal("expected direction error")
	}
}
