package jsonnames

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"unicode"
)

var (
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	fieldCache      sync.Map // reflect.Type -> immutable map[string]reflect.Type
)

func structuralType(value reflect.Type) reflect.Type {
	for value != nil {
		if value.Implements(unmarshalerType) || (value.Kind() != reflect.Pointer && reflect.PointerTo(value).Implements(unmarshalerType)) {
			return nil
		}
		if value.Kind() != reflect.Pointer {
			return value
		}
		value = value.Elem()
	}
	return nil
}

type fieldCandidate struct {
	typeOf    reflect.Type
	depth     int
	tagged    bool
	ambiguous bool
}

// JSON promotion follows field depth and then explicit-tag dominance, not Go
// identifier visibility. Equal candidates are ambiguous and accept no name.
func jsonFields(value reflect.Type) map[string]reflect.Type {
	if cached, exists := fieldCache.Load(value); exists {
		return cached.(map[string]reflect.Type)
	}
	candidates := make(map[string]fieldCandidate)
	collectFields(value, 0, make(map[reflect.Type]bool), candidates)
	fields := make(map[string]reflect.Type, len(candidates))
	for name, candidate := range candidates {
		if !candidate.ambiguous {
			fields[name] = candidate.typeOf
		}
	}
	cached, _ := fieldCache.LoadOrStore(value, fields)
	return cached.(map[string]reflect.Type)
}

func collectFields(value reflect.Type, depth int, ancestors map[reflect.Type]bool, fields map[string]fieldCandidate) {
	if ancestors[value] {
		return
	}
	ancestors[value] = true
	defer delete(ancestors, value)
	for index := range value.NumField() {
		field := value.Field(index)
		underlying := field.Type
		if underlying.Kind() == reflect.Pointer {
			underlying = underlying.Elem()
		}
		if field.PkgPath != "" && (!field.Anonymous || underlying.Kind() != reflect.Struct) {
			continue
		}
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if !validTagName(tag) {
			tag = ""
		}
		if field.Anonymous && tag == "" && underlying.Kind() == reflect.Struct {
			collectFields(underlying, depth+1, ancestors, fields)
			continue
		}
		name := tag
		if name == "" {
			name = field.Name
		}
		candidate := fieldCandidate{typeOf: field.Type, depth: depth, tagged: tag != ""}
		previous, exists := fields[name]
		switch {
		case !exists || depth < previous.depth || (depth == previous.depth && candidate.tagged && !previous.tagged):
			fields[name] = candidate
		case depth == previous.depth && candidate.tagged == previous.tagged:
			previous.ambiguous = true
			fields[name] = previous
		}
	}
}

func validTagName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", character) && !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}
