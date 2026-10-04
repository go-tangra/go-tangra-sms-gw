// Package ratelimit is the public listener's keyed token-bucket limiter
// (login per client address, send per client, receipts per address). It is
// in-process, like the source; the key map is bounded with LRU eviction so
// rotating keys cannot exhaust memory.
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// Limiter holds one token bucket per key. Safe for concurrent use.
type Limiter struct {
	rate    float64 // tokens per second
	burst   float64
	maxKeys int
	now     func() time.Time

	mu    sync.Mutex
	keys  map[string]*list.Element
	order *list.List // front = most recently used
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// New allows perMinute requests per key with the given burst; maxKeys
// bounds the tracked keys (default 10000).
func New(perMinute float64, burst, maxKeys int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = 10000
	}
	return &Limiter{rate: perMinute / 60, burst: float64(burst), maxKeys: maxKeys, now: time.Now, keys: map[string]*list.Element{}, order: list.New()}
}

// Allow takes one token from key's bucket; an empty key shares one bucket.
func (l *Limiter) Allow(key string) bool { return l.take(key, 1) }

// Available reports whether key's bucket holds a token without taking it.
func (l *Limiter) Available(key string) bool { return l.take(key, 0) }

func (l *Limiter) take(key string, n float64) bool {
	if key == "" {
		key = "_anon"
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if e, ok := l.keys[key]; ok {
		l.order.MoveToFront(e)
		b = e.Value.(*bucket)
		b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
	} else {
		b = &bucket{key: key, tokens: l.burst, last: now}
		l.keys[key] = l.order.PushFront(b)
		if l.order.Len() > l.maxKeys {
			old := l.order.Back()
			l.order.Remove(old)
			delete(l.keys, old.Value.(*bucket).key)
		}
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens -= n
	return true
}
