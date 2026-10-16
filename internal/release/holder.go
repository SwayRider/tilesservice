package release

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/pmtiles"
)

// loadTimeout bounds opening a release (pointer, manifest, archive header and root directory).
const loadTimeout = 30 * time.Second

// Release is one loaded release. Use it only between Acquire and the returned release func.
type Release struct {
	Key      string // identity of the release in its store (changes when the pointer moves)
	Manifest *Manifest
	Archive  *pmtiles.Reader

	loc      Location
	loadedAt time.Time
}

// ReadFile returns a small file of the release (style, glyph range, sprite).
func (r *Release) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	return r.loc.ReadFile(ctx, rel)
}

// slot tracks the requests still using a release, so a replaced release is closed only after
// the last of them finishes.
type slot struct {
	rel     *Release
	refs    int
	retired bool
}

// Status is the readiness view of a holder; it is also the body of the readiness endpoint.
type Status struct {
	Loaded        bool              `json:"loaded"`
	Release       string            `json:"release,omitempty"`
	Key           string            `json:"key,omitempty"`
	TilesetBuild  string            `json:"tileset_build,omitempty"`
	TilesetDate   string            `json:"tileset_date,omitempty"`
	SchemaVersion string            `json:"schema_version,omitempty"`
	Styles        map[string]string `json:"styles,omitempty"` // style id -> version
	LoadedAt      *time.Time        `json:"loaded_at,omitempty"`
	LastCheck     *time.Time        `json:"last_check,omitempty"`
	LastError     string            `json:"last_error,omitempty"` // empty when the last check succeeded
}

// Holder keeps the active release and switches to a newer one atomically: requests that already
// hold the old release finish on it, new requests see the new one. A failed load keeps the old
// release and is reported in Status.
type Holder struct {
	store Store
	l     *log.Logger

	reloadMu sync.Mutex // one reload at a time

	mu        sync.Mutex
	cur       *slot
	lastCheck time.Time
	lastErr   error
	closed    bool

	trigger chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

// NewHolder creates an empty holder; call Reload (and Start) to load a release.
func NewHolder(store Store, l *log.Logger) *Holder {
	return &Holder{
		store:   store,
		l:       l.Derive(log.WithComponent("ReleaseHolder")),
		trigger: make(chan struct{}, 1),
	}
}

// Acquire returns the active release and a func that must be called when the caller is done with
// it. It returns nil and a no-op func when no release is loaded.
func (h *Holder) Acquire() (*Release, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return nil, func() {}
	}
	s := h.cur
	s.refs++
	var once sync.Once
	return s.rel, func() { once.Do(func() { h.unref(s) }) }
}

func (h *Holder) unref(s *slot) {
	h.mu.Lock()
	s.refs--
	closeNow := s.retired && s.refs == 0
	h.mu.Unlock()
	if closeNow {
		h.closeRelease(s.rel)
	}
}

func (h *Holder) closeRelease(r *Release) {
	if err := r.Archive.Close(); err != nil {
		h.l.Warnf("closing archive of release %s: %v", r.Manifest.Release, err)
	}
}

// Reload checks the pointer and switches to the release it names when that differs from the
// active one. It reports whether a switch happened. On error the active release stays.
func (h *Holder) Reload(ctx context.Context) (bool, error) {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()

	changed, err := h.reload(ctx)

	h.mu.Lock()
	h.lastCheck = time.Now()
	h.lastErr = err
	h.mu.Unlock()
	return changed, err
}

func (h *Holder) reload(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()

	key, loc, err := h.store.Current(ctx)
	if err != nil {
		return false, err
	}

	h.mu.Lock()
	same := h.cur != nil && h.cur.rel.Key == key
	h.mu.Unlock()
	if same {
		return false, nil
	}

	data, err := loc.ReadFile(ctx, ManifestFile)
	if err != nil {
		return false, fmt.Errorf("release %s: reading %s: %w", key, ManifestFile, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return false, fmt.Errorf("release %s: %w", key, err)
	}
	archive, err := loc.OpenArchive(ctx, m.Tileset.File)
	if err != nil {
		return false, fmt.Errorf("release %s: opening %s: %w", key, m.Tileset.File, err)
	}

	next := &slot{rel: &Release{Key: key, Manifest: m, Archive: archive, loc: loc, loadedAt: time.Now()}}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		h.closeRelease(next.rel)
		return false, fmt.Errorf("release: holder is closed")
	}
	old := h.cur
	h.cur = next
	closeOld := false
	if old != nil {
		old.retired = true
		closeOld = old.refs == 0
	}
	h.mu.Unlock()

	if old != nil {
		h.l.Infof("switched from release %s to %s", old.rel.Manifest.Release, m.Release)
		if closeOld {
			h.closeRelease(old.rel)
		}
	} else {
		h.l.Infof("loaded release %s (tileset build %s, %d styles)", m.Release, m.Tileset.Build, len(m.Styles))
	}
	return true, nil
}

// Status reports what is loaded and how the last check went.
func (h *Holder) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	var st Status
	if !h.lastCheck.IsZero() {
		t := h.lastCheck
		st.LastCheck = &t
	}
	if h.lastErr != nil {
		st.LastError = h.lastErr.Error()
	}
	if h.cur != nil {
		r := h.cur.rel
		t := r.loadedAt
		st.Loaded = true
		st.Release = r.Manifest.Release
		st.Key = r.Key
		st.TilesetBuild = r.Manifest.Tileset.Build
		st.TilesetDate = r.Manifest.Tileset.Date
		st.SchemaVersion = r.Manifest.Tileset.SchemaVersion
		st.Styles = r.Manifest.StyleVersions()
		st.LoadedAt = &t
	}
	return st
}

// Trigger asks the poller to reload now (for SIGHUP). It never blocks.
func (h *Holder) Trigger() {
	select {
	case h.trigger <- struct{}{}:
	default:
	}
}

// Start runs the poller: it reloads every interval (0 = only on Trigger) until Close.
func (h *Holder) Start(interval time.Duration) {
	h.mu.Lock()
	if h.stop != nil || h.closed {
		h.mu.Unlock()
		return
	}
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	stop, done := h.stop, h.done
	h.mu.Unlock()

	go func() {
		defer close(done)
		var tick <-chan time.Time
		if interval > 0 {
			t := time.NewTicker(interval)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-stop:
				return
			case <-tick:
			case <-h.trigger:
				h.l.Infoln("reload requested")
			}
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			if _, err := h.Reload(ctx); err != nil {
				h.l.Errorf("reload failed, keeping the current release: %v", err)
			}
			cancel()
		}
	}()
}

// Close stops the poller and closes the active release once its requests finish.
func (h *Holder) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	stop, done := h.stop, h.done
	h.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}

	// Wait for a reload that is still running, then retire the active release.
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	h.mu.Lock()
	s := h.cur
	h.cur = nil
	closeNow := false
	if s != nil {
		s.retired = true
		closeNow = s.refs == 0
	}
	h.mu.Unlock()
	if closeNow {
		h.closeRelease(s.rel)
	}
}
