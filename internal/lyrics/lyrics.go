// Package lyrics fetches the words of a track from a public provider and returns
// them in the shape the LavaLyrics plugin uses, so a client that already reads
// lyrics from a Lavalink node running that plugin can read them from this one.
package lyrics

import (
	"context"
	"errors"
	"sync"
	"time"

	"golalink/internal/protocol"
)

// Line is one line of lyrics, timed from the start of the track.
type Line struct {
	Timestamp int64           `json:"timestamp"`
	Duration  *int64          `json:"duration"`
	Line      string          `json:"line"`
	Plugin    protocol.Object `json:"plugin"`
}

// Lyrics is what a provider returns for a track. SourceName is where the track
// came from and Provider is who supplied the words, which is the pair the plugin
// reports to a client.
type Lyrics struct {
	SourceName string          `json:"sourceName"`
	Provider   string          `json:"provider"`
	Text       *string         `json:"text"`
	Lines      []Line          `json:"lines"`
	Plugin     protocol.Object `json:"plugin"`
}

// ErrNotFound reports that a provider has no lyrics for a track. It is an answer
// rather than a failure, and the caller replies with no content.
var ErrNotFound = errors.New("lyrics: no lyrics for this track")

// Provider fetches the lyrics of one track.
type Provider interface {
	// Name is the provider name reported to the client.
	Name() string
	// Fetch returns the lyrics, or ErrNotFound when the provider has none.
	Fetch(ctx context.Context, t *protocol.Track) (*Lyrics, error)
}

// DefaultTTL is how long a fetched result is reused. Lyrics do not change, so the
// cache is there to keep a client that asks twice from turning into two calls to
// a free public service, not to keep up with anything.
const DefaultTTL = time.Hour

// cacheLimit bounds the cache, so a node that runs for weeks does not hold every
// track it ever played.
const cacheLimit = 256

// Manager asks the configured providers in order and remembers what they said.
type Manager struct {
	providers []Provider
	ttl       time.Duration
	limit     int

	mu    sync.Mutex
	cache map[string]cacheEntry
	order []string
}

type cacheEntry struct {
	lyrics *Lyrics
	at     time.Time
}

// New builds a manager over the providers, which are asked in the order given.
func New(providers ...Provider) *Manager {
	return &Manager{
		providers: providers,
		ttl:       DefaultTTL,
		limit:     cacheLimit,
		cache:     make(map[string]cacheEntry),
	}
}

// Enabled reports whether any provider is configured, so a caller can answer
// without asking.
func (m *Manager) Enabled() bool {
	return m != nil && len(m.providers) > 0
}

// Fetch returns the lyrics for a track, or ErrNotFound when no provider has any.
// A provider that fails is passed over rather than reported straight away,
// because the next one may still answer; only when none of them do is the last
// failure raised.
func (m *Manager) Fetch(ctx context.Context, t *protocol.Track) (*Lyrics, error) {
	if !m.Enabled() || t == nil {
		return nil, ErrNotFound
	}
	key := t.Info.SourceName + "\x00" + t.Info.Identifier
	if v, ok := m.cached(key); ok {
		if v == nil {
			return nil, ErrNotFound
		}
		return v, nil
	}
	var last error
	for _, p := range m.providers {
		l, err := p.Fetch(ctx, t)
		if err == nil {
			m.remember(key, l)
			return l, nil
		}
		if !errors.Is(err, ErrNotFound) {
			last = err
		}
	}
	// A provider that failed is not the same as one that has nothing. The failure
	// may pass, so it is reported rather than remembered as a miss: remembering it
	// would answer every request for that track from the cache for the rest of the
	// hour, including the ones made after the service came back.
	if last != nil {
		return nil, last
	}
	m.remember(key, nil)
	return nil, ErrNotFound
}

func (m *Manager) cached(key string) (*Lyrics, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.cache[key]
	if !ok {
		return nil, false
	}
	if time.Since(v.at) > m.ttl {
		delete(m.cache, key)
		return nil, false
	}
	return v.lyrics, true
}

// remember stores a result, including a miss, so a track nobody has lyrics for
// is not looked up again on every request. Entries leave in the order they
// arrived, which is enough to keep the map bounded.
func (m *Manager) remember(key string, l *Lyrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cache[key]; !ok {
		if len(m.order) >= m.limit {
			delete(m.cache, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, key)
	}
	m.cache[key] = cacheEntry{lyrics: l, at: time.Now()}
}
