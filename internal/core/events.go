package core

import (
	"sync"
	"time"
)

// Event is pushed to connected browsers over Server-Sent Events.
type Event struct {
	Type    string `json:"type"`              // mail | sync | signout | queue
	Folder  string `json:"folder,omitempty"`  // folder that changed
	From    string `json:"from,omitempty"`    // for new-mail notifications
	Subject string `json:"subject,omitempty"` //
	Thread  int64  `json:"thread,omitempty"`  //
}

// Hub fans events out to every open tab of a user.
type Hub struct {
	mu   sync.Mutex
	subs map[int64]map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[int64]map[chan Event]struct{}{}} }

func (h *Hub) Subscribe(userID int64) chan Event {
	ch := make(chan Event, 32)
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[chan Event]struct{}{}
	}
	h.subs[userID][ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(userID int64, ch chan Event) {
	h.mu.Lock()
	delete(h.subs[userID], ch)
	if len(h.subs[userID]) == 0 {
		delete(h.subs, userID)
	}
	h.mu.Unlock()
}

// Publish never blocks: slow clients simply miss events and resync.
func (h *Hub) Publish(userID int64, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[userID] {
		select {
		case ch <- ev:
		default:
		}
	}
}

// RateLimiter is a small fixed-window counter keyed by string.
type RateLimiter struct {
	mu sync.Mutex
	m  map[string]*rlEntry
}

type rlEntry struct {
	count int
	reset time.Time
}

func NewRateLimiter() *RateLimiter { return &RateLimiter{m: map[string]*rlEntry{}} }

// Allow records one hit for key and reports whether it is within limit per window.
func (r *RateLimiter) Allow(key string, limit int, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	e := r.m[key]
	if e == nil || now.After(e.reset) {
		e = &rlEntry{reset: now.Add(window)}
		r.m[key] = e
	}
	e.count++
	if len(r.m) > 50000 { // crude memory bound
		for k, v := range r.m {
			if now.After(v.reset) {
				delete(r.m, k)
			}
		}
	}
	return e.count <= limit
}

// Blocked reports whether key has exceeded limit without recording a hit.
func (r *RateLimiter) Blocked(key string, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[key]
	return e != nil && time.Now().Before(e.reset) && e.count >= limit
}

// Refund takes back one hit recorded for key.
func (r *RateLimiter) Refund(key string) {
	r.mu.Lock()
	if e := r.m[key]; e != nil && e.count > 0 {
		e.count--
	}
	r.mu.Unlock()
}

func (r *RateLimiter) Reset(key string) {
	r.mu.Lock()
	delete(r.m, key)
	r.mu.Unlock()
}
