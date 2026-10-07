// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package claw

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is a claw job file: a type, and whatever that type needs.
//
// One file rather than a flag per application, because a generic command with
// per-type flags is not a generic command — `larri claw` would grow a
// --workflow the day something wanted one and a --checkpoint the day something
// else did. The type owns everything below `type:` and decodes it itself.
//
//	type: comfyui
//	workflow: ./w.json
//	models: ./models.yaml
//
// It is also the artefact worth version-controlling. A job is a repeatable
// thing — the same graph, the same pins, the same hardware floor — and a
// command line is not somewhere that survives.
type Config struct {
	// Type is what the file declares. A --type flag may supply it when the
	// file omits it, and disagreeing with it is an error.
	Type Type

	// Dir is the directory holding the file. Relative paths inside the config
	// resolve against it — see Resolve.
	Dir string

	// Path is the file itself, for error messages.
	Path string

	// doc is the whole document, kept so a Kind can decode its own shape out
	// of it without this package knowing any of the fields.
	doc yaml.Node
}

// LoadConfig reads a claw job file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("claw: config %s: %w", path, err)
	}
	return ParseConfig(path, b)
}

// ParseConfig reads a claw job file from bytes, attributing it to path.
func ParseConfig(path string, b []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("claw: config %s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("claw: config %s is empty", path)
	}
	var head struct {
		Type Type `yaml:"type"`
	}
	if err := doc.Decode(&head); err != nil {
		return nil, fmt.Errorf("claw: config %s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		dir = filepath.Dir(path)
	}
	return &Config{Type: head.Type, Dir: dir, Path: path, doc: doc}, nil
}

// Decode unmarshals the whole document into a type-specific struct.
//
// The Kind defines the shape; this package never learns it. Unknown fields are
// rejected, because a mistyped key in a job file is otherwise a setting that
// silently does not apply — and for a file that decides what hardware to rent,
// silence is the wrong outcome.
func (c *Config) Decode(into any) error {
	if c == nil {
		return fmt.Errorf("claw: no config")
	}
	// Through a Decoder rather than yaml.Node.Decode, because KnownFields is
	// an option on the former and there is no equivalent on a node. The `type`
	// key is dropped on the way so that a Kind's struct describes its own
	// fields and nothing else.
	raw, err := yaml.Marshal(withoutType(&c.doc))
	if err != nil {
		return fmt.Errorf("claw: config %s: %w", c.Path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("claw: config %s: %w", c.Path, err)
	}
	return nil
}

// withoutType copies the document's mapping without the `type` key, which
// belongs to this layer rather than to the Kind decoding the rest.
func withoutType(doc *yaml.Node) *yaml.Node {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return doc
	}
	m := *doc.Content[0]
	m.Content = nil
	src := doc.Content[0].Content
	for i := 0; i+1 < len(src); i += 2 {
		if src[i].Value == "type" {
			continue
		}
		m.Content = append(m.Content, src[i], src[i+1])
	}
	return &m
}

// Resolve turns a path from the config into one relative to the config file.
//
// A job file that only works from one working directory is a job file that
// breaks the first time it runs anywhere else — in CI, from an editor, or from
// a sibling directory. Absolute paths and empty values are returned unchanged.
func (c *Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if c == nil || c.Dir == "" {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// WithType settles the type from the file and the flag.
//
// Either may supply it and both may, but disagreeing is refused rather than
// resolved: a file saying one thing and a flag saying another means one of them
// is a mistake, and guessing which would run the wrong application against
// somebody's config.
func (c *Config) WithType(flag Type) error {
	switch {
	case c.Type == "" && flag == "":
		return fmt.Errorf("claw: config %s declares no type: add one, or pass --type", c.Path)
	case c.Type == "":
		c.Type = flag
	case flag != "" && flag != c.Type:
		return fmt.Errorf("claw: config %s declares type %q, --type says %q", c.Path, c.Type, flag)
	}
	return nil
}
