package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema constrains a response to a JSON object matching a JSON Schema.
//
// Set Request.Schema and the provider is told to emit an instance of JSON
// rather than prose; Response.Text then holds that JSON object as a string,
// ready to hand to json.Unmarshal. Leaving it nil is free-form text and is
// byte-for-byte the request this package sent before schemas existed.
//
// Why this is worth having at all: asking a model for JSON in the prompt and
// parsing what comes back fails often enough to matter. Measured against
// claude-sonnet-5 by replaying one real prompt, roughly 8% of unconstrained
// responses were malformed - the model stopped mid-string with a perfectly
// ordinary stop_reason. The same prompt through the constrained path failed
// 0 times in 174. That gap is the whole reason this exists.
//
// The JSON must use the narrow subset that means the same thing on all three
// providers - see validateSchema, which enforces it locally before any HTTP so
// a schema that would work on one backend can't silently misbehave on another:
//
//   - the root must be an object
//   - types: object, array, string, integer, number, boolean
//   - every node may carry "description"; nothing else beyond the per-type
//     keywords below
//   - object: "properties", "required" listing every property exactly once,
//     and "additionalProperties": false
//   - array: "items"
//   - string and integer: an optional non-empty "enum"
//
// Anything else is rejected rather than forwarded. That includes length and
// numeric bounds (minLength, maxItems, minimum...): OpenAI enforces them, xAI
// enforces them up to documented limits, and Anthropic's tool input_schema
// accepts them and just doesn't constrain decoding with them - so a Request
// carrying one would mean three different things. Nullable fields and union
// types are documented on all three but left out because nothing needs them
// yet; both are additive later.
//
// One trap worth knowing about "enum": it constrains the provider, not you. A
// conforming model picks a value off the list, so a variant your own parser
// doesn't recognise still arrives as valid JSON of the declared type and passes
// every check here - and then whatever tolerant unmarshalling you have drops it
// with no error anywhere. The reverse is just as quiet: a value your parser
// accepts but the enum doesn't offer becomes impossible for the model to
// produce, so that case silently never happens again. enum is the one part of
// the subset a caller can hold wrong, and it is wrong invisibly in both
// directions - so derive it from the parser's own set of values rather than
// writing the list out twice.
//
// What is deliberately not checked here: anything a provider would reject
// loudly. The test for whether a rule belongs in this validator is whether
// getting it wrong fails silently. Semantic divergence does, which is the whole
// reason for the subset. Provider quotas - OpenAI's limits on nesting depth,
// property count and so on - do not: they come back as a 400 carrying the real
// limit. Checking them here would replace an accurate error with a guess at it,
// and because this runs inside Complete with no way to opt out, a wrong guess
// blocks a schema the provider would have accepted.
type Schema struct {
	// Name identifies the schema to the provider. Required, and it must match
	// schemaNamePattern - Anthropic documents that regex for a tool name and
	// OpenAI documents the same character set and 64-byte cap for a
	// response-format name, so one rule satisfies both.
	Name string
	// Description tells the model what the schema is for. Optional, and passed
	// through to every provider so the field doesn't mean something different
	// depending on who answers.
	Description string
	// JSON is the JSON Schema itself, as a raw JSON object.
	JSON json.RawMessage
}

// schemaNamePattern is the intersection of the providers' name rules:
// Anthropic documents ^[a-zA-Z0-9_-]{1,64}$ for a tool name, and OpenAI
// documents "a-z, A-Z, 0-9, or contain underscores and dashes, with a maximum
// length of 64" for a response-format name.
const schemaNamePattern = `^[a-zA-Z0-9_-]{1,64}$`

var schemaNameRE = regexp.MustCompile(schemaNamePattern)

// schemaKeywords maps each supported type to the keywords allowed alongside
// "type" and "description" on a node of that type. Its keys double as the list
// of supported types.
//
// This is an allowlist rather than a blocklist of known-bad keywords on
// purpose: JSON Schema has a large vocabulary and the providers each support a
// different slice of it, so anything not named here is something nobody has
// checked. A loud rejection beats a keyword one provider honors and another
// ignores.
//
// Adding a keyword here means writing the code that reads its value, so know
// this before you do: json.Unmarshal does not fail on a JSON null. It leaves
// the destination at its zero value and returns nil, so any check that leans on
// the unmarshal error to reject a wrong-typed value accepts null silently -
// which is how a `"additionalProperties": null` once read as false. Decode a
// string through jsonString and anything else through an explicit isJSONNull
// check; don't hand a raw value straight to json.Unmarshal.
var schemaKeywords = map[string][]string{
	"object":  {"properties", "required", "additionalProperties"},
	"array":   {"items"},
	"string":  {"enum"},
	"integer": {"enum"},
	"number":  nil,
	"boolean": nil,
}

// supportedSchemaTypes renders schemaKeywords' keys for an error message, in a
// stable order - map iteration is random and a reshuffling error string is
// miserable to read.
var supportedSchemaTypes = func() string {
	got := make([]string, 0, len(schemaKeywords))
	for t := range schemaKeywords {
		got = append(got, t)
	}
	sort.Strings(got)
	return strings.Join(got, ", ")
}()

// validateSchema enforces the cross-provider schema contract, locally, before
// any HTTP - the same bargain the rest of validate makes. A schema the module
// forwarded blindly would be a 400 on one provider, a silently unenforced
// keyword on another, and a working constraint on a third.
func validateSchema(s *Schema) error {
	if !schemaNameRE.MatchString(s.Name) {
		return fmt.Errorf("llm: Schema.Name %q must match %s", s.Name, schemaNamePattern)
	}
	if len(s.JSON) == 0 {
		return fmt.Errorf("llm: Schema.JSON is required")
	}

	// The root type is checked here rather than in the recursion because it's
	// the one rule that isn't about the node itself: every provider requires
	// the top level of a constrained response to be an object.
	var root struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(s.JSON, &root); err != nil {
		return fmt.Errorf("llm: Schema.JSON must be a JSON object: %w", err)
	}
	if root.Type != "object" {
		return fmt.Errorf("llm: Schema.JSON must have a root %q of \"object\", got %q", "type", root.Type)
	}

	return validateSchemaNode(s.JSON, "#")
}

// validateSchemaNode checks one node and everything below it. path is a
// JSON-pointer-ish trail ("#/properties/entries/items") so an error names the
// offending node in a schema that may be several levels deep.
func validateSchemaNode(raw json.RawMessage, path string) error {
	node, ok := decodeJSONObject(raw)
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s must be a JSON object", path)
	}

	rawType, ok := node["type"]
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s is missing %q", path, "type")
	}
	typ, ok := jsonString(rawType)
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s has a non-string %q: %s", path, "type", rawType)
	}
	extra, supported := schemaKeywords[typ]
	if !supported {
		return fmt.Errorf("llm: Schema.JSON: %s has unsupported type %q (supported: %s)", path, typ, supportedSchemaTypes)
	}

	allowed := map[string]bool{"type": true, "description": true}
	for _, k := range extra {
		allowed[k] = true
	}
	var unknown []string
	for k := range node {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("llm: Schema.JSON: %s has unsupported keyword(s) %s on a %q (allowed: %s)",
			path, strings.Join(quoteAll(unknown), ", "), typ, strings.Join(quoteAll(sortedKeys(allowed)), ", "))
	}

	// description is allowed everywhere and forwarded verbatim, so it has to be
	// a string here or the providers each decide for themselves what a
	// non-string one means.
	if rawDesc, ok := node["description"]; ok {
		if _, ok := jsonString(rawDesc); !ok {
			return fmt.Errorf("llm: Schema.JSON: %s has a non-string %q: %s", path, "description", rawDesc)
		}
	}

	switch typ {
	case "object":
		return validateSchemaObject(node, path)
	case "array":
		items, ok := node["items"]
		if !ok {
			return fmt.Errorf("llm: Schema.JSON: %s is an array with no %q", path, "items")
		}
		return validateSchemaNode(items, path+"/items")
	case "string", "integer":
		return validateSchemaEnum(node, typ, path)
	}
	return nil
}

// validateSchemaObject applies the three object rules. All three exist because
// a provider demands them: OpenAI's strict mode rejects an object that omits
// additionalProperties: false or leaves a property out of "required", and xAI
// documents the same additionalProperties requirement (inverting the JSON
// Schema default to boot). Anthropic's tool input_schema asks for neither, so
// enforcing them here is what makes one Schema portable.
func validateSchemaObject(node map[string]json.RawMessage, path string) error {
	rawAP, ok := node["additionalProperties"]
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s is missing %q: false", path, "additionalProperties")
	}
	var ap bool
	if isJSONNull(rawAP) || json.Unmarshal(rawAP, &ap) != nil || ap {
		return fmt.Errorf("llm: Schema.JSON: %s must set %q to false, got %s", path, "additionalProperties", rawAP)
	}

	rawProps, ok := node["properties"]
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s is missing %q", path, "properties")
	}
	props, ok := decodeJSONObject(rawProps)
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s has a %q that is not a JSON object", path, "properties")
	}
	if len(props) == 0 {
		return fmt.Errorf("llm: Schema.JSON: %s has no properties", path)
	}

	rawRequired, ok := node["required"]
	if !ok {
		return fmt.Errorf("llm: Schema.JSON: %s is missing %q", path, "required")
	}
	// Decoded element by element rather than straight into []string, because
	// json.Unmarshal turns a null element into "" without complaining - so
	// "required": [null] would silently require a property named "".
	var elems []json.RawMessage
	if isJSONNull(rawRequired) || json.Unmarshal(rawRequired, &elems) != nil {
		return fmt.Errorf("llm: Schema.JSON: %s has a %q that is not an array of strings: %s", path, "required", rawRequired)
	}
	required := make([]string, len(elems))
	for i, e := range elems {
		name, ok := jsonString(e)
		if !ok {
			return fmt.Errorf("llm: Schema.JSON: %s %s[%d] is not a string: %s", path, "required", i, e)
		}
		required[i] = name
	}

	// "required" must name every property exactly once. Reporting both
	// directions separately matters: a missing name is a property the model may
	// omit, and a stray one names a property that doesn't exist - different
	// mistakes with different fixes.
	set := make(map[string]bool, len(required))
	for _, name := range required {
		if set[name] {
			return fmt.Errorf("llm: Schema.JSON: %s lists %q twice in %q", path, name, "required")
		}
		set[name] = true
	}
	var missing, stray []string
	for name := range props {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	for name := range set {
		if _, ok := props[name]; !ok {
			stray = append(stray, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("llm: Schema.JSON: %s omits %s from %q (every property must be required)",
			path, strings.Join(quoteAll(missing), ", "), "required")
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return fmt.Errorf("llm: Schema.JSON: %s requires %s, which it does not define as a property",
			path, strings.Join(quoteAll(stray), ", "))
	}

	// Sorted so a schema with two bad properties always reports the same one.
	for _, name := range sortedKeys(props) {
		if err := validateSchemaNode(props[name], path+"/properties/"+name); err != nil {
			return err
		}
	}
	return nil
}

// validateSchemaEnum checks an optional enum. An empty one is rejected because
// xAI documents a zero-variant enum as a 400 and it can't match anything
// anyway; values are checked against the declared type because OpenAI and xAI
// reject a mismatched schema outright and Anthropic would just quietly ignore
// it.
func validateSchemaEnum(node map[string]json.RawMessage, typ, path string) error {
	raw, ok := node["enum"]
	if !ok {
		return nil
	}
	var values []json.RawMessage
	if isJSONNull(raw) || json.Unmarshal(raw, &values) != nil {
		return fmt.Errorf("llm: Schema.JSON: %s has an %q that is not an array: %s", path, "enum", raw)
	}
	if len(values) == 0 {
		return fmt.Errorf("llm: Schema.JSON: %s has an empty %q", path, "enum")
	}
	for i, v := range values {
		var err error
		switch {
		case isJSONNull(v):
			err = errNotJSONNull
		case typ == "string":
			var s string
			err = json.Unmarshal(v, &s)
		case typ == "integer":
			var n int64
			err = json.Unmarshal(v, &n)
		}
		if err != nil {
			return fmt.Errorf("llm: Schema.JSON: %s %s[%d] is %s, not %s", path, "enum", i, v, typ)
		}
	}
	return nil
}

// errNotJSONNull only ever reaches the caller's own error message; it exists so
// the null case can share the mistyped-value branch.
var errNotJSONNull = errors.New("null")

// isJSONNull reports whether raw is a literal JSON null.
//
// Every type check here leans on json.Unmarshal failing for a wrong-typed
// value, and null is the one value that doesn't: it unmarshals into anything,
// leaves the zero value, and returns nil. So a `"additionalProperties": null`
// would read as false and an `"enum": [null]` as a valid variant, both silently.
// Nullable values are deliberately outside the portable subset (see Schema), so
// they have to be caught by hand.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// jsonString decodes a JSON string, rejecting null for the reason above. Every
// string-valued keyword in the subset goes through here so that "absent",
// "null", and "" stay three different things.
func jsonString(raw json.RawMessage) (string, bool) {
	if isJSONNull(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// decodeJSONObject reports whether raw is a JSON object and hands back its keys
// with their values still raw, so unknown-keyword detection sees exactly what
// the caller wrote. JSON null decodes without error into a nil map, which is
// why the nil check is here rather than only at the call sites.
func decodeJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func sortedKeys[V any](m map[string]V) []string {
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	return got
}

func quoteAll(names []string) []string {
	got := make([]string, len(names))
	for i, n := range names {
		got[i] = fmt.Sprintf("%q", n)
	}
	return got
}
