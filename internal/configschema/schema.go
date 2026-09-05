// Package configschema verifies and applies plugin-owned JSON Schemas.
package configschema

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	FileName = "config.schema.json"
	MaxBytes = 1 << 20
)

type Declaration struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (d Declaration) Validate() error {
	if d.Source != FileName || d.Size < 1 || d.Size > MaxBytes {
		return errors.New("invalid configuration schema declaration")
	}
	digest, err := hex.DecodeString(d.SHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != d.SHA256 {
		return errors.New("invalid configuration schema declaration")
	}
	return nil
}

type Schema struct{ compiled *jsonschema.Schema }

func Compile(data []byte) (*Schema, error) {
	if len(data) < 1 || len(data) > MaxBytes {
		return nil, errors.New("configuration schema exceeds its size limit")
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noExternalLoader{})
	if err := compiler.AddResource(FileName, document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(FileName)
	if err != nil {
		return nil, err
	}
	return &Schema{compiled: compiled}, nil
}

func Load(root string, declaration Declaration) (*Schema, error) {
	if !filepath.IsAbs(root) || declaration.Validate() != nil {
		return nil, errors.New("invalid configuration schema")
	}
	path := filepath.Join(root, filepath.FromSlash(declaration.Source))
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != declaration.Size {
		return nil, errors.New("invalid configuration schema")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("invalid configuration schema")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, after) || int64(len(data)) != declaration.Size {
		return nil, errors.New("invalid configuration schema")
	}
	want, _ := hex.DecodeString(declaration.SHA256)
	digest := sha256.Sum256(data)
	if subtle.ConstantTimeCompare(digest[:], want) != 1 {
		return nil, errors.New("invalid configuration schema")
	}
	return Compile(data)
}

func (s *Schema) Validate(data []byte) error {
	if s == nil || s.compiled == nil {
		return errors.New("configuration schema is unavailable")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return s.compiled.Validate(value)
}

type noExternalLoader struct{}

func (noExternalLoader) Load(string) (any, error) {
	return nil, errors.New("external JSON Schema references are unsupported")
}
