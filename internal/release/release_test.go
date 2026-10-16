package release_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
	"github.com/swayrider/tilesservice/internal/release"
	"github.com/swayrider/tilesservice/internal/tilesource"
)

func testLogger() *log.Logger { return log.New(log.WithComponent("test")) }

func manifestJSON(id, build string) string {
	return fmt.Sprintf(`{"schema":1,"release":%q,
 "tileset":{"name":"planet","build":%q,"date":"2026-10-04","schema_version":"4.15.2","file":"tiles.pmtiles"},
 "styles":[{"id":"classic","label":"Classic","version":3,"default":true,
   "variants":{"light":"styles/classic/3/light.json","dark":"styles/classic/3/dark.json"}}]}`, id, build)
}

func archiveBytes(tag string) []byte {
	return pmtilestest.Build([]pmtilestest.Tile{
		{Z: 0, X: 0, Y: 0, Data: pmtilestest.Gzip([]byte("tile-" + tag))},
	}, pmtilestest.Options{GzipDirectories: true})
}

// writeRelease writes a release directory with a manifest and an archive tagged tag.
func writeRelease(t *testing.T, dir, id, build, tag string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "styles", "classic", "3"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("manifest.json", []byte(manifestJSON(id, build)))
	must("tiles.pmtiles", archiveBytes(tag))
	must("styles/classic/3/light.json", []byte(`{"version":8}`))
}

// point makes root/current a relative symlink to target (replacing it atomically).
func point(t *testing.T, root, target string) {
	t.Helper()
	tmp := filepath.Join(root, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
}

func fileHolder(t *testing.T, root string) *release.Holder {
	t.Helper()
	store, err := release.OpenStore(root, tilesource.S3{})
	if err != nil {
		t.Fatal(err)
	}
	h := release.NewHolder(store, testLogger())
	t.Cleanup(h.Close)
	return h
}

// gunzip decodes a stored (gzip) tile.
func gunzip(data []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	plain, err := io.ReadAll(zr)
	return string(plain), err
}

func tileOf(t *testing.T, rel *release.Release) string {
	t.Helper()
	data, err := rel.Archive.Tile(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gunzip(data)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestParseManifest(t *testing.T) {
	m, err := release.ParseManifest([]byte(manifestJSON("r-1", "20261004")))
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "r-1" || m.Tileset.Build != "20261004" || m.Styles[0].Version != "3" {
		t.Fatalf("unexpected manifest %+v", m)
	}
	if got := m.StyleVersions()["classic"]; got != "3" {
		t.Fatalf("style version %q", got)
	}
	// the version may also be written as a string
	m, err = release.ParseManifest([]byte(strings.Replace(manifestJSON("r-1", "b"), `"version":3`, `"version":"3"`, 1)))
	if err != nil || m.Styles[0].Version != "3" {
		t.Fatalf("string version: %v %+v", err, m)
	}
}

func TestParseManifest_Rejects(t *testing.T) {
	good := manifestJSON("r-1", "b")
	cases := map[string]string{
		"not json":        `{`,
		"wrong schema":    strings.Replace(good, `"schema":1`, `"schema":2`, 1),
		"empty release":   strings.Replace(good, `"release":"r-1"`, `"release":""`, 1),
		"release slash":   strings.Replace(good, `"release":"r-1"`, `"release":"a/b"`, 1),
		"tileset name":    strings.Replace(good, `"name":"planet"`, `"name":"world"`, 1),
		"reserved name":   strings.Replace(good, `"name":"planet"`, `"name":"styles"`, 1),
		"file escapes":    strings.Replace(good, `"file":"tiles.pmtiles"`, `"file":"../x.pmtiles"`, 1),
		"file absolute":   strings.Replace(good, `"file":"tiles.pmtiles"`, `"file":"/etc/passwd"`, 1),
		"file empty":      strings.Replace(good, `"file":"tiles.pmtiles"`, `"file":""`, 1),
		"style id":        strings.Replace(good, `"id":"classic"`, `"id":"a/b"`, 1),
		"style version":   strings.Replace(good, `"version":3`, `"version":".."`, 1),
		"variant name":    strings.Replace(good, `"light":`, `"sepia":`, 1),
		"variant escapes": strings.Replace(good, `"styles/classic/3/dark.json"`, `"../../dark.json"`, 1),
		"two defaults":    strings.Replace(good, `"styles":[`, `"styles":[{"id":"b","version":1,"default":true,"variants":{"light":"b.json"}},`, 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := release.ParseManifest([]byte(doc)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidRel(t *testing.T) {
	for _, ok := range []string{"a", "a/b.json", "styles/classic/3/light.json", "glyphs/Noto Sans Regular/0-255.pbf"} {
		if err := release.ValidRel(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/a", "a/", "a//b", "../a", "a/../b", "a/./b", "./a", "a\\b", "a\x00b", ".."} {
		if release.ValidRel(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHolder_LoadsFileRelease(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "20261001", "one")
	point(t, root, "releases/r-1/tiles")

	h := fileHolder(t, root)
	if rel, done := h.Acquire(); rel != nil {
		done()
		t.Fatal("a release before the first Reload")
	}
	changed, err := h.Reload(context.Background())
	if err != nil || !changed {
		t.Fatalf("Reload = %v, %v", changed, err)
	}
	rel, done := h.Acquire()
	defer done()
	if rel == nil || rel.Manifest.Release != "r-1" || tileOf(t, rel) != "tile-one" {
		t.Fatalf("unexpected release %+v", rel)
	}
	style, err := rel.ReadFile(context.Background(), "styles/classic/3/light.json")
	if err != nil || string(style) != `{"version":8}` {
		t.Fatalf("ReadFile = %q, %v", style, err)
	}
	if _, err := rel.ReadFile(context.Background(), "../outside"); err == nil {
		t.Fatal("ReadFile accepted a path outside the release")
	}

	st := h.Status()
	if !st.Loaded || st.Release != "r-1" || st.TilesetBuild != "20261001" || st.SchemaVersion != "4.15.2" ||
		st.Styles["classic"] != "3" || st.LastError != "" || st.LastCheck == nil {
		t.Fatalf("status %+v", st)
	}

	// unchanged pointer: no switch
	if changed, err := h.Reload(context.Background()); err != nil || changed {
		t.Fatalf("second Reload = %v, %v", changed, err)
	}
}

func TestHolder_SwitchAndRollback(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "20261001", "one")
	writeRelease(t, filepath.Join(root, "releases", "r-2", "tiles"), "r-2", "20261002", "two")
	point(t, root, "releases/r-1/tiles")
	h := fileHolder(t, root)
	ctx := context.Background()
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// a request in flight on r-1 keeps working after the switch to r-2
	old, doneOld := h.Acquire()
	point(t, root, "releases/r-2/tiles")
	if changed, err := h.Reload(ctx); err != nil || !changed {
		t.Fatalf("switch = %v, %v", changed, err)
	}
	if got := tileOf(t, old); got != "tile-one" {
		t.Fatalf("in-flight request lost its release: %q", got)
	}
	doneOld()
	doneOld() // calling the release func twice is harmless
	if _, err := old.Archive.Tile(0, 0, 0); err == nil {
		t.Fatal("the retired archive stayed open after its last request finished")
	}

	cur, done := h.Acquire()
	if tileOf(t, cur) != "tile-two" || cur.Manifest.Release != "r-2" {
		t.Fatal("new requests do not see r-2")
	}
	done()

	// rollback
	point(t, root, "releases/r-1/tiles")
	if changed, err := h.Reload(ctx); err != nil || !changed {
		t.Fatalf("rollback = %v, %v", changed, err)
	}
	cur, done = h.Acquire()
	defer done()
	if tileOf(t, cur) != "tile-one" {
		t.Fatal("rollback did not restore r-1")
	}
}

func TestHolder_BrokenReleaseKeepsTheOldOne(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "20261001", "one")
	point(t, root, "releases/r-1/tiles")
	h := fileHolder(t, root)
	ctx := context.Background()
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	broken := map[string]func(dir string){
		"missing manifest": func(dir string) { _ = os.Remove(filepath.Join(dir, "manifest.json")) },
		"bad manifest":     func(dir string) { _ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{"), 0o644) },
		"missing archive":  func(dir string) { _ = os.Remove(filepath.Join(dir, "tiles.pmtiles")) },
		"corrupt archive": func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "tiles.pmtiles"), []byte("not a pmtiles file"), 0o644)
		},
	}
	for name, breakIt := range broken {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(root, "releases", "bad", "tiles")
			_ = os.RemoveAll(filepath.Join(root, "releases", "bad"))
			writeRelease(t, dir, "bad", "20261009", "bad")
			breakIt(dir)
			point(t, root, "releases/bad/tiles")

			if changed, err := h.Reload(ctx); err == nil || changed {
				t.Fatalf("Reload = %v, %v; want an error", changed, err)
			}
			cur, done := h.Acquire()
			defer done()
			if cur == nil || cur.Manifest.Release != "r-1" || tileOf(t, cur) != "tile-one" {
				t.Fatal("a broken release replaced the working one")
			}
			st := h.Status()
			if !st.Loaded || st.Release != "r-1" || st.LastError == "" {
				t.Fatalf("status does not report the failed reload: %+v", st)
			}
		})
	}

	// fixing the pointer clears the error
	point(t, root, "releases/r-1/tiles")
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.Status(); st.LastError != "" {
		t.Fatalf("error not cleared: %+v", st)
	}
}

func TestHolder_NoReleaseYet(t *testing.T) {
	root := t.TempDir() // no current symlink
	h := fileHolder(t, root)
	if _, err := h.Reload(context.Background()); err == nil {
		t.Fatal("expected an error without a current pointer")
	}
	if st := h.Status(); st.Loaded || st.LastError == "" {
		t.Fatalf("status %+v", st)
	}
	// it loads once the release appears
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "b", "one")
	point(t, root, "releases/r-1/tiles")
	if changed, err := h.Reload(context.Background()); err != nil || !changed {
		t.Fatalf("Reload = %v, %v", changed, err)
	}
}

func TestHolder_PollFollowsThePointer(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "b1", "one")
	writeRelease(t, filepath.Join(root, "releases", "r-2", "tiles"), "r-2", "b2", "two")
	point(t, root, "releases/r-1/tiles")
	h := fileHolder(t, root)
	if _, err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Start(20 * time.Millisecond)
	point(t, root, "releases/r-2/tiles")
	waitFor(t, func() bool { return h.Status().Release == "r-2" })
}

func TestHolder_TriggerReloadsWithoutPolling(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "b1", "one")
	writeRelease(t, filepath.Join(root, "releases", "r-2", "tiles"), "r-2", "b2", "two")
	point(t, root, "releases/r-1/tiles")
	h := fileHolder(t, root)
	if _, err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Start(0) // SIGHUP only
	point(t, root, "releases/r-2/tiles")
	time.Sleep(50 * time.Millisecond)
	if h.Status().Release != "r-1" {
		t.Fatal("switched without a trigger")
	}
	h.Trigger()
	waitFor(t, func() bool { return h.Status().Release == "r-2" })
}

// A switch while requests run: every request sees a complete, consistent release and no read
// fails. Run with -race.
func TestHolder_SwitchUnderLoad(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "b1", "one")
	writeRelease(t, filepath.Join(root, "releases", "r-2", "tiles"), "r-2", "b2", "two")
	point(t, root, "releases/r-1/tiles")
	h := fileHolder(t, root)
	ctx := context.Background()
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var failures atomic.Int64
	var reads atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				rel, done := h.Acquire()
				data, err := rel.Archive.Tile(0, 0, 0)
				plain, gerr := gunzip(data)
				want := map[string]string{"r-1": "tile-one", "r-2": "tile-two"}[rel.Manifest.Release]
				if err != nil || gerr != nil || plain != want {
					failures.Add(1)
				}
				reads.Add(1)
				done()
			}
		}()
	}
	for i := range 40 {
		point(t, root, fmt.Sprintf("releases/r-%d/tiles", i%2+1))
		if _, err := h.Reload(ctx); err != nil {
			t.Error(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if failures.Load() != 0 || reads.Load() == 0 {
		t.Fatalf("%d failed reads out of %d", failures.Load(), reads.Load())
	}
}

func TestHolder_CloseStopsPollerAndClosesRelease(t *testing.T) {
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "releases", "r-1", "tiles"), "r-1", "b1", "one")
	point(t, root, "releases/r-1/tiles")
	store, _ := release.OpenStore(root, tilesource.S3{})
	h := release.NewHolder(store, testLogger())
	if _, err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Start(10 * time.Millisecond)
	rel, done := h.Acquire()
	h.Close()
	h.Close() // idempotent
	if _, err := rel.Archive.Tile(0, 0, 0); err != nil {
		t.Fatal("Close closed a release that a request still holds")
	}
	done()
	if _, err := rel.Archive.Tile(0, 0, 0); err == nil {
		t.Fatal("the archive stayed open after Close and the last release")
	}
	if r, d := h.Acquire(); r != nil {
		d()
		t.Fatal("Acquire returned a release after Close")
	}
	if _, err := h.Reload(context.Background()); err == nil {
		t.Fatal("Reload worked on a closed holder")
	}
}

// --- object store -------------------------------------------------------------------------

// fakeStore is an in-memory path-style S3 bucket with HEAD and ranged GET.
type fakeStore struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
}

func (f *fakeStore) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	data, ok := f.objects[strings.TrimPrefix(r.URL.Path, "/"+f.bucket+"/")]
	f.mu.Unlock()
	if !ok || !strings.HasPrefix(r.URL.Path, "/"+f.bucket+"/") {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		return
	}
	var start, end int
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start > end || end >= len(data) {
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func putS3Release(f *fakeStore, prefix, id, build, tag string) {
	f.put(prefix+"manifest.json", []byte(manifestJSON(id, build)))
	f.put(prefix+"tiles.pmtiles", archiveBytes(tag))
	f.put(prefix+"styles/classic/3/light.json", []byte(`{"version":8}`))
}

func putPointer(f *fakeStore, base, id string) {
	doc, _ := json.Marshal(map[string]any{"schema": 1, "release": id, "prefix": "releases/" + id + "/", "updated_at": "2026-10-10T00:00:00Z"})
	f.put(base+"current.json", doc)
}

func TestS3Store(t *testing.T) {
	for _, base := range []string{"", "tiles/"} {
		t.Run("base="+base, func(t *testing.T) {
			f := &fakeStore{bucket: "swayrider-tiles", objects: map[string][]byte{}}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			putS3Release(f, base+"releases/r-1/", "r-1", "b1", "one")
			putS3Release(f, base+"releases/r-2/", "r-2", "b2", "two")
			putPointer(f, base, "r-1")

			location := "s3://swayrider-tiles"
			if base != "" {
				location += "/" + strings.TrimSuffix(base, "/")
			}
			store, err := release.OpenStore(location, tilesource.S3{Endpoint: srv.URL, Region: "garage"})
			if err != nil {
				t.Fatal(err)
			}
			h := release.NewHolder(store, testLogger())
			t.Cleanup(h.Close)
			ctx := context.Background()
			if changed, err := h.Reload(ctx); err != nil || !changed {
				t.Fatalf("Reload = %v, %v", changed, err)
			}
			rel, done := h.Acquire()
			if rel.Manifest.Release != "r-1" || tileOf(t, rel) != "tile-one" {
				t.Fatal("did not load r-1")
			}
			style, err := rel.ReadFile(ctx, "styles/classic/3/light.json")
			if err != nil || string(style) != `{"version":8}` {
				t.Fatalf("ReadFile = %q, %v", style, err)
			}
			done()

			if changed, err := h.Reload(ctx); err != nil || changed {
				t.Fatalf("unchanged pointer: %v, %v", changed, err)
			}
			putPointer(f, base, "r-2")
			if changed, err := h.Reload(ctx); err != nil || !changed {
				t.Fatalf("switch = %v, %v", changed, err)
			}
			rel, done = h.Acquire()
			defer done()
			if rel.Manifest.Release != "r-2" || tileOf(t, rel) != "tile-two" {
				t.Fatal("did not switch to r-2")
			}

			// a pointer to a release that is not there leaves r-2 active
			putPointer(f, base, "r-9")
			if _, err := h.Reload(ctx); err == nil {
				t.Fatal("expected an error for a missing release")
			}
			if st := h.Status(); st.Release != "r-2" || st.LastError == "" {
				t.Fatalf("status %+v", st)
			}
		})
	}
}

func TestS3Store_BadPointer(t *testing.T) {
	f := &fakeStore{bucket: "b", objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	store, err := release.OpenStore("s3://b", tilesource.S3{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"not json":       `{`,
		"schema":         `{"schema":2,"prefix":"releases/r-1/"}`,
		"empty prefix":   `{"schema":1,"prefix":""}`,
		"escaping":       `{"schema":1,"prefix":"../other/"}`,
		"inner escaping": `{"schema":1,"prefix":"releases/../../x/"}`,
	} {
		f.put("current.json", []byte(doc))
		if _, _, err := store.Current(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	delete(f.objects, "current.json")
	if _, _, err := store.Current(context.Background()); err == nil {
		t.Error("missing pointer: expected an error")
	}
}

func TestOpenStore_Errors(t *testing.T) {
	for _, loc := range []string{"", "s3://", "s3://bucket", "file://relative/path"} {
		if _, err := release.OpenStore(loc, tilesource.S3{}); err == nil {
			t.Errorf("OpenStore(%q) succeeded", loc)
		}
	}
	if _, err := release.OpenStore("file:///data/tiles", tilesource.S3{}); err != nil {
		t.Errorf("file:// location rejected: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
