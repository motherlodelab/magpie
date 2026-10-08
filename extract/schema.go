package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// FieldHints carries parsed x-magpie extensions per JSON-pointer field path.
type FieldHints struct {
	Coerce     map[string]string // field path -> coerce kind
	CSSHint    map[string]string
	Regex      map[string]string
	JSONLDPath map[string]string
	Trim       map[string]bool
	Multiple   map[string]bool
}

// Schema bundles the compiled validator with its hints.
type Schema struct {
	Raw       any
	Validator *jsonschema.Schema
	Hints     FieldHints
}

// LoadSchema loads YAML or JSON draft 2020-12, compiles, parses x-magpie.
func LoadSchema(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("extract: read schema %s: %w", path, err)
	}
	return ParseSchema(data)
}

var errExternalRef = errors.New("magpie doesn't load other files or URLs")

// noExternalRefs refuses every external $ref (D1): a schema is untrusted
// input (desktop renderer, MCP inline schema), and the default loader reads
// local files. Internal "#/..." refs and the draft metaschemas never get here.
type noExternalRefs struct{}

// Load receives the RESOLVED url (a relative ref arrives as file:///<cwd>/…).
func (noExternalRefs) Load(u string) (any, error) {
	return nil, fmt.Errorf("schema $ref %q points outside this schema — %w.\n"+
		"Fix: copy that definition into this schema under \"$defs\" (e.g. \"$defs\": {\"money\": {...}}) "+
		"and point to it with \"$ref\": \"#/$defs/money\"", u, errExternalRef)
}

// ParseSchema parses in-memory schema bytes (YAML or JSON).
func ParseSchema(data []byte) (*Schema, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("extract: parse schema: %w", err)
	}
	v = yamlToJSON(v)
	rawJSON, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("extract: marshal schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noExternalRefs{})
	if err := c.AddResource("schema.json", v); err != nil {
		return nil, fmt.Errorf("extract: add schema: %w", err)
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		// LoadURLError has no Unwrap: surface the refusal alone so its fix
		// line isn't buried under the compiler's "failing loading" wrapper.
		var le *jsonschema.LoadURLError
		if errors.As(err, &le) && errors.Is(le.Err, errExternalRef) {
			return nil, fmt.Errorf("extract: %w", le.Err)
		}
		return nil, fmt.Errorf("extract: compile schema: %w", err)
	}
	s := &Schema{Raw: v, Validator: sch, Hints: FieldHints{
		Coerce: map[string]string{}, CSSHint: map[string]string{},
		Regex: map[string]string{}, JSONLDPath: map[string]string{},
		Trim: map[string]bool{}, Multiple: map[string]bool{},
	}}
	if err := s.parseHints(v, ""); err != nil {
		return nil, err
	}
	_ = rawJSON
	return s, nil
}

func yamlToJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = yamlToJSON(e)
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = yamlToJSON(e)
		}
		return t
	default:
		return v
	}
}

var knownCoerce = map[string]bool{
	"int": true, "float": true, "eur_decimal": true,
	"iso_date": true, "bool": true, "trim": true, "regex": true,
}

func (s *Schema) parseHints(node any, path string) error {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if xg, ok := obj["x-magpie"].(map[string]any); ok {
		field := strings.TrimPrefix(path, "/properties/")
		if str, ok := xg["coerce"].(string); ok && str != "" {
			if !knownCoerce[str] {
				return fmt.Errorf("extract: unknown x-magpie.coerce %q at %s", str, path)
			}
			s.Hints.Coerce[field] = str
		}
		if str, ok := xg["css_hint"].(string); ok {
			s.Hints.CSSHint[field] = str
		}
		if str, ok := xg["regex"].(string); ok {
			s.Hints.Regex[field] = str
		}
		if str, ok := xg["jsonld_path"].(string); ok {
			s.Hints.JSONLDPath[field] = str
		}
		if b, ok := xg["trim"].(bool); ok && b {
			s.Hints.Trim[field] = true
		}
		if b, ok := xg["multiple"].(bool); ok && b {
			s.Hints.Multiple[field] = true
		}
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		for name, sub := range props {
			if err := s.parseHints(sub, path+"/properties/"+name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks doc bytes against the schema. Returns the raw error
// (a *jsonschema.ValidationError) so the repair loop can embed err.Error()
// verbatim — v6 renders "- at '/price': got string, want number".
func (s *Schema) Validate(doc []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("extract: parse output: %w", err)
	}
	if err := s.Validator.Validate(inst); err != nil {
		return err
	}
	return nil
}

// JSONLDWalk resolves a minimal $.a.b.0.c path against sidecar JSON.
func JSONLDWalk(sidecar json.RawMessage, path string) (any, bool) {
	if len(sidecar) == 0 || !strings.HasPrefix(path, "$.") {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(sidecar, &v); err != nil {
		return nil, false
	}
	// If sidecar is an array of blocks, search each.
	cands := []any{v}
	if arr, ok := v.([]any); ok {
		cands = arr
	}
	for _, c := range cands {
		if out, ok := walkOne(c, strings.Split(strings.TrimPrefix(path, "$."), ".")); ok {
			return out, true
		}
	}
	return nil, false
}

func walkOne(v any, parts []string) (any, bool) {
	for _, p := range parts {
		switch t := v.(type) {
		case map[string]any:
			nv, ok := t[p]
			if !ok {
				return nil, false
			}
			v = nv
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}
