package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := New(10, 5, 0) // 10/min = one token every 6 s
	l.now = func() time.Time { return now }
	for i := range 5 {
		if !l.Allow("ip") {
			t.Fatalf("request %d limited", i)
		}
	}
	if l.Allow("ip") || l.Allow("ip") {
		t.Fatal("burst exceeded")
	}
	if !l.Allow("other") {
		t.Fatal("keys share a bucket")
	}
	now = now.Add(6 * time.Second)
	if !l.Allow("ip") || l.Allow("ip") {
		t.Fatal("refill is not one token per 6 s")
	}
	now = now.Add(time.Hour)
	for range 5 {
		l.Allow("ip")
	}
	if l.Allow("ip") {
		t.Fatal("refill exceeded the burst")
	}
}

func TestKeyMapIsBounded(t *testing.T) {
	l := New(1, 1, 3)
	for _, k := range []string{"a", "b", "c", "d"} {
		l.Allow(k)
	}
	if len(l.keys) != 3 || l.order.Len() != 3 {
		t.Fatalf("%d keys", len(l.keys))
	}
	if _, ok := l.keys["a"]; ok {
		t.Fatal("least recently used key kept")
	}
	if l.Allow("") != true || l.Allow("") != false {
		t.Fatal("empty key is not one shared bucket")
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := New(0.0001, 20, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("client:1") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 20 {
		t.Fatalf("%d allowed", allowed)
	}
}

func TestAvailableDoesNotTake(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := New(1, 2, 0)
	l.now = func() time.Time { return now }
	for range 5 {
		if !l.Available("ip") {
			t.Fatal("peek consumed a token")
		}
	}
	l.Allow("ip")
	l.Allow("ip")
	if l.Available("ip") {
		t.Fatal("empty bucket reported available")
	}
}
