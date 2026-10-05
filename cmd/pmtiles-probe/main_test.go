package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
)

func TestProbeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.pmtiles")
	file := pmtilestest.Build([]pmtilestest.Tile{{Z: 2, X: 1, Y: 1, Data: pmtilestest.Gzip([]byte("x"))}},
		pmtilestest.Options{Metadata: `{"name":"T","version":"1","vector_layers":[{"id":"roads"},{"id":"water"}]}`})
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	none := func(string) string { return "" }

	var out bytes.Buffer
	outTile := filepath.Join(t.TempDir(), "tile.bin")
	if err := run([]string{"-url", path, "-tile", "2/1/1", "-out", outTile}, none, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zoom:              2-2", "layers: roads, water", "tile 2/1/1:", "written to"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if b, err := os.ReadFile(outTile); err != nil || !bytes.Equal(b, pmtilestest.Gzip([]byte("x"))) {
		t.Errorf("written tile: %v", err)
	}

	out.Reset()
	if err := run([]string{"-url", path, "-tile", "2/0/0"}, none, &out); err != nil || !strings.Contains(out.String(), "not in the archive") {
		t.Errorf("missing tile: %v\n%s", err, out.String())
	}
	if err := run([]string{"-tile", "2/0/0"}, none, &out); err == nil {
		t.Error("no url must fail")
	}
	if err := run([]string{"-url", path, "-tile", "bad"}, none, &out); err == nil {
		t.Error("bad tile must fail")
	}
}
