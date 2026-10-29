package main

import (
	"sync"
	"time"
)

const (
	// A page reload drops the live stream for a moment; only exit once it stays gone.
	closeGrace = 4 * time.Second
	// Exit if no window ever connects, e.g. when no browser could be opened.
	connectTimeout = 60 * time.Second
)

// lifetime ends once no window has been connected for a grace period. A newer instance
// can take the windows over with MoveTo, after which they leave and this one ends.
type lifetime struct {
	mu      sync.Mutex
	clients int
	timer   *time.Timer
	done    chan struct{}
	once    sync.Once
	moved   chan struct{} // closed once the windows should go to movedTo
	movedTo string
}

func newLifetime() *lifetime {
	l := &lifetime{done: make(chan struct{}), moved: make(chan struct{})}
	l.timer = time.AfterFunc(connectTimeout, l.expire)
	return l
}

func (l *lifetime) Connect() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clients++
	l.timer.Stop()
}

func (l *lifetime) Disconnect() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clients--
	if l.clients == 0 {
		l.timer.Reset(closeGrace)
	}
}

func (l *lifetime) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clients == 0 {
		l.once.Do(func() { close(l.done) })
	}
}

func (l *lifetime) Done() <-chan struct{} { return l.done }

// MoveTo sends every connected window to url and reports how many there were.
func (l *lifetime) MoveTo(url string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.movedTo == "" {
		l.movedTo = url
		close(l.moved)
	}
	return l.clients
}

// Moved is closed once the windows should leave for MovedTo.
func (l *lifetime) Moved() <-chan struct{} { return l.moved }

func (l *lifetime) MovedTo() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.movedTo
}
