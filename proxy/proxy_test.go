package proxy_test

import (
	"io"
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
