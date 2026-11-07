package api

import (
	"sync"
	"time"
)

// limiter counts failed logins per key (IP or username) in a fixed window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*window
	now    func() time.Time
}

type window struct {
	start time.Time
	count int
}

func newLimiter(max int, w time.Duration) *limiter {
	return &limiter{max: max, window: w, hits: map[string]*window{}, now: time.Now}
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.hits[key]
	if !ok {
		return false
	}
	if l.now().Sub(h.start) > l.window {
		delete(l.hits, key)
		return false
	}
	return h.count >= l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	// Drop expired entries now and then so the map cannot grow without bound.
	if len(l.hits) > 1000 {
		for k, h := range l.hits {
			if t.Sub(h.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	h, ok := l.hits[key]
	if !ok || t.Sub(h.start) > l.window {
		l.hits[key] = &window{start: t, count: 1}
		return
	}
	h.count++
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
