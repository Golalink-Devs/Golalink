package lyrics

import (
	"context"
	"errors"
	"testing"

	"golalink/internal/protocol"
)

// track builds the part of a protocol track a provider reads. The identifier and
// source name together are what the manager caches on, so a case that wants a
// distinct track only has to change the identifier.
func track(source, id, author, title string, length int64) *protocol.Track {
	return &protocol.Track{Info: protocol.TrackInfo{
		SourceName: source,
		Identifier: id,
		Author:     author,
		Title:      title,
		Length:     length,
	}}
}

// stubProvider answers with a fixed result and counts how often it was asked, so
// a case can prove which provider answered and whether the manager skipped the
// rest.
type stubProvider struct {
	name   string
	lyrics *Lyrics
	err    error
	calls  int
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Fetch(context.Context, *protocol.Track) (*Lyrics, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.lyrics, nil
}

func TestManagerTakesTheFirstAnswer(t *testing.T) {
	first := &Lyrics{Provider: "first"}
	second := &Lyrics{Provider: "second"}
	a, b := &stubProvider{name: "a", lyrics: first}, &stubProvider{name: "b", lyrics: second}

	got, err := New(a, b).Fetch(context.Background(), track("youtube", "id", "Artist", "Song", 0))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != first {
		t.Fatalf("answered from %q, want the first provider's answer", got.Provider)
	}
	if b.calls != 0 {
		t.Fatalf("the second provider was asked %d times after the first answered", b.calls)
	}
}

func TestManagerPassesOverFailures(t *testing.T) {
	// A provider that is down must not cost the track its lyrics when the next
	// one can still answer, and a provider with nothing is not a failure at all.
	down := &stubProvider{name: "down", err: errors.New("connection refused")}
	empty := &stubProvider{name: "empty", err: ErrNotFound}
	want := &Lyrics{Provider: "up"}
	up := &stubProvider{name: "up", lyrics: want}

	got, err := New(down, empty, up).Fetch(context.Background(), track("youtube", "id", "", "", 0))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != want {
		t.Fatalf("got %v, want the third provider's answer", got)
	}
	for _, p := range []*stubProvider{down, empty, up} {
		if p.calls != 1 {
			t.Fatalf("%s was asked %d times, want once", p.name, p.calls)
		}
	}
}

func TestManagerReportsTheLastFailure(t *testing.T) {
	missing := &stubProvider{name: "missing", err: ErrNotFound}
	first := errors.New("first failure")
	second := errors.New("second failure")
	a := &stubProvider{name: "a", err: first}
	b := &stubProvider{name: "b", err: second}

	ctx := context.Background()
	tr := track("youtube", "id", "", "", 0)
	m := New(missing, a, b)
	_, err := m.Fetch(ctx, tr)
	if !errors.Is(err, second) {
		t.Fatalf("got %v, want the last failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a failure was reported as a track with no lyrics")
	}
	// A failure may pass, so it is not remembered as a miss: the providers are
	// asked again rather than the track being answered from cache for an hour.
	if _, err := m.Fetch(ctx, tr); !errors.Is(err, second) {
		t.Fatalf("second call: got %v, want the same failure", err)
	}
	if a.calls != 2 || b.calls != 2 {
		t.Fatalf("providers were asked %d and %d times after two failed calls, want twice each", a.calls, b.calls)
	}
}

func TestManagerRemembers(t *testing.T) {
	ctx := context.Background()
	tr := track("youtube", "id", "", "", 0)

	t.Run("an answer is served from the cache", func(t *testing.T) {
		want := &Lyrics{Provider: "a"}
		p := &stubProvider{name: "a", lyrics: want}
		m := New(p)
		for i := 0; i < 3; i++ {
			got, err := m.Fetch(ctx, tr)
			if err != nil || got != want {
				t.Fatalf("call %d: got %v, %v", i, got, err)
			}
		}
		if p.calls != 1 {
			t.Fatalf("the provider was asked %d times, want once", p.calls)
		}
	})

	t.Run("a miss is remembered", func(t *testing.T) {
		p := &stubProvider{name: "a", err: ErrNotFound}
		m := New(p)
		for i := 0; i < 3; i++ {
			if _, err := m.Fetch(ctx, tr); !errors.Is(err, ErrNotFound) {
				t.Fatalf("call %d: got %v, want ErrNotFound", i, err)
			}
		}
		if p.calls != 1 {
			t.Fatalf("the provider was asked %d times, want once", p.calls)
		}
	})

	t.Run("a failure is not remembered", func(t *testing.T) {
		p := &stubProvider{name: "a", err: errors.New("boom")}
		m := New(p)
		for i := 0; i < 2; i++ {
			if _, err := m.Fetch(ctx, tr); err == nil {
				t.Fatalf("call %d: a failed provider answered", i)
			}
		}
		if p.calls != 2 {
			t.Fatalf("the provider was asked %d times, want twice", p.calls)
		}
	})

	t.Run("an expired answer is fetched again", func(t *testing.T) {
		p := &stubProvider{name: "a", lyrics: &Lyrics{Provider: "a"}}
		m := New(p)
		// A negative lifetime makes the entry stale the moment it is stored, so
		// the expiry is exercised without waiting for it.
		m.ttl = -1
		for i := 0; i < 2; i++ {
			if _, err := m.Fetch(ctx, tr); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if p.calls != 2 {
			t.Fatalf("the provider was asked %d times, want twice", p.calls)
		}
	})
}

// TestManagerEvictsTheOldestEntry covers the bound on the cache: a node that runs
// for weeks must not hold every track it ever played.
func TestManagerEvictsTheOldestEntry(t *testing.T) {
	p := &stubProvider{name: "a", err: ErrNotFound}
	m := New(p)
	m.limit = 2
	ctx := context.Background()

	ask := func(id string, wantCalls int) {
		t.Helper()
		if _, err := m.Fetch(ctx, track("youtube", id, "", "", 0)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: got %v, want ErrNotFound", id, err)
		}
		if p.calls != wantCalls {
			t.Fatalf("after %s the provider had been asked %d times, want %d", id, p.calls, wantCalls)
		}
	}
	ask("one", 1)
	ask("two", 2)
	ask("one", 2) // remembered, so the provider is not asked again
	// The third entry pushes out the one that arrived first.
	ask("three", 3)
	// "two" arrived after "one", so it is the one still remembered.
	ask("two", 3)
	ask("one", 4)
}

// TestManagerWithoutAProviderIsAnAnswer keeps the caller from having to ask
// whether lyrics are configured before asking for them.
func TestManagerWithoutAProviderIsAnAnswer(t *testing.T) {
	ctx := context.Background()
	tr := track("youtube", "id", "", "", 0)

	if New().Enabled() {
		t.Fatal("a manager with no provider reported itself enabled")
	}
	if _, err := New().Fetch(ctx, tr); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}

	var off *Manager
	if off.Enabled() {
		t.Fatal("a nil manager reported itself enabled")
	}
	if _, err := off.Fetch(ctx, tr); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil manager: got %v, want ErrNotFound", err)
	}

	p := &stubProvider{name: "a", lyrics: &Lyrics{Provider: "a"}}
	if _, err := New(p).Fetch(ctx, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil track: got %v, want ErrNotFound", err)
	}
	if p.calls != 0 {
		t.Fatalf("the provider was asked about a nil track")
	}
}
