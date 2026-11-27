package proxy

import (
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func TestSeededDelayAndDrop(t *testing.T) {
	const n = 32
	a := sampleDelays(rand.New(rand.NewSource(42)), 20*time.Millisecond, 5*time.Millisecond, n)
	b := sampleDelays(rand.New(rand.NewSource(42)), 20*time.Millisecond, 5*time.Millisecond, n)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same seed produced different delays\n%v\n%v", a, b)
	}
	varied := false
	for _, d := range a {
		if d < 15*time.Millisecond || d > 25*time.Millisecond {
			t.Fatalf("delay %s outside [15ms, 25ms]", d)
		}
		if d != a[0] {
			varied = true
		}
	}
	if !varied {
		t.Fatal("jitter did not change the delay")
	}

	dropsA := sampleDrops(rand.New(rand.NewSource(7)), 0.5, 64)
	dropsB := sampleDrops(rand.New(rand.NewSource(7)), 0.5, 64)
	if !reflect.DeepEqual(dropsA, dropsB) {
		t.Fatal("same seed produced different drops")
	}
	sawDrop, sawKeep := false, false
	for _, drop := range dropsA {
		if drop {
			sawDrop = true
		} else {
			sawKeep = true
		}
	}
	if !sawDrop || !sawKeep {
		t.Fatalf("drop sample was uniform: %v", dropsA)
	}
	if randDrop(nil, 0.5) {
		t.Fatal("nil rng dropped a partial rate")
	}
	if !randDrop(nil, 1) {
		t.Fatal("nil rng kept a certain drop")
	}
}

func sampleDelays(rng *rand.Rand, latency, jitter time.Duration, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = randDelay(rng, latency, jitter)
	}
	return out
}

func sampleDrops(rng *rand.Rand, rate float64, n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = randDrop(rng, rate)
	}
	return out
}
