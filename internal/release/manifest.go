// Package release loads the tiles release written by the data-manager: a PMTiles archive plus a
// manifest, styles, glyphs and sprites, found through a pointer that names the active release.
// A Holder keeps the active release and switches to a newer one without dropping requests.
package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// PlanetTileset is the tileset name the manifest must carry.
const PlanetTileset = "planet"

// ManifestSchema is the manifest schema version this code reads.
const ManifestSchema = 1

// ManifestFile is the manifest's path relative to the release.
const ManifestFile = "manifest.json"

var (
	validID      = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	validVersion = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)
	validRelease = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)
)

// reservedTilesets cannot be a tileset name: they are literal routes under /v1/tiles/.
var reservedTilesets = map[string]bool{"styles": true, "fonts": true, "sprites": true, "ping": true, "ready": true}

// Manifest describes one release (schema 1).
type Manifest struct {
	Schema  int     `json:"schema"`
	Release string  `json:"release"`
	Tileset Tileset `json:"tileset"`
	Styles  []Style `json:"styles"`
}

// Tileset describes the PMTiles archive of a release.
type Tileset struct {
	Name          string `json:"name"`
	Build         string `json:"build"`
	Date          string `json:"date"`
	SchemaVersion string `json:"schema_version"`
	File          string `json:"file"` // relative to the release
}

// Style is one named style with a light and a dark variant.
type Style struct {
	ID       string            `json:"id"`
	Label    string            `json:"label"`
	Version  FlexString        `json:"version"`
	Default  bool              `json:"default"`
	Variants map[string]string `json:"variants"` // "light"/"dark" -> path relative to the release
}

// FlexString reads a JSON string or number as a string (the style version is written as an
// integer by some producers and as a string by others).
type FlexString string

// UnmarshalJSON accepts "3" and 3.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("version must be a string or a number: %w", err)
	}
	*f = FlexString(n.String())
	return nil
}

// ParseManifest decodes and validates a manifest.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("release: manifest is not valid JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest; every path in it must stay inside the release.
func (m *Manifest) Validate() error {
	if m.Schema != ManifestSchema {
		return fmt.Errorf("release: manifest schema %d is not supported (want %d)", m.Schema, ManifestSchema)
	}
	if !validRelease.MatchString(m.Release) {
		return fmt.Errorf("release: invalid release id %q", m.Release)
	}
	if reservedTilesets[m.Tileset.Name] {
		return fmt.Errorf("release: tileset name %q is reserved", m.Tileset.Name)
	}
	if m.Tileset.Name != PlanetTileset {
		return fmt.Errorf("release: tileset name %q is not supported (want %q)", m.Tileset.Name, PlanetTileset)
	}
	if err := ValidRel(m.Tileset.File); err != nil {
		return fmt.Errorf("release: tileset file: %w", err)
	}

	seen := map[string]bool{}
	defaults := 0
	for i, s := range m.Styles {
		if !validID.MatchString(s.ID) {
			return fmt.Errorf("release: style %d: invalid id %q", i, s.ID)
		}
		if !validVersion.MatchString(string(s.Version)) || s.Version == "." || s.Version == ".." {
			return fmt.Errorf("release: style %q: invalid version %q", s.ID, s.Version)
		}
		key := s.ID + "/" + string(s.Version)
		if seen[key] {
			return fmt.Errorf("release: style %q version %q listed twice", s.ID, s.Version)
		}
		seen[key] = true
		if len(s.Variants) == 0 {
			return fmt.Errorf("release: style %q has no variants", s.ID)
		}
		for name, p := range s.Variants {
			if name != "light" && name != "dark" {
				return fmt.Errorf("release: style %q: unknown variant %q", s.ID, name)
			}
			if err := ValidRel(p); err != nil {
				return fmt.Errorf("release: style %q variant %q: %w", s.ID, name, err)
			}
		}
		if s.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return errors.New("release: more than one default style")
	}
	return nil
}

// StyleVersions maps style id to version, for status reporting.
func (m *Manifest) StyleVersions() map[string]string {
	out := make(map[string]string, len(m.Styles))
	for _, s := range m.Styles {
		out[s.ID] = string(s.Version)
	}
	return out
}

// ValidRel checks that p is a clean relative path that stays inside the release directory:
// no empty path, no absolute path, no backslashes, no "." or ".." segments, no double slashes.
func ValidRel(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case strings.ContainsAny(p, "\\\x00"):
		return fmt.Errorf("path %q contains an invalid character", p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q must be relative", p)
	case path.Clean(p) != p:
		return fmt.Errorf("path %q is not clean", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("path %q leaves the release", p)
		}
	}
	return nil
}

// itoa is a small helper for error messages.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
