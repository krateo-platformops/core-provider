package chart

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/krateo-platformops/plumbing/valuesschema"
)

// #568 child 7 (Diego, 2026-11-22): core-provider OWNS the inferred-schema marker.
//
// The portal keys its "this schema was inferred" banner on the root keyword valuesschema.MarkerKey
// (x-krateo-values-schema) in the schema core-provider publishes, because both of its forms already
// read that ConfigMap. The contract is that the marker is present IF AND ONLY IF the spec schema was
// inferred (ValuesSchemaAuthored=False/InferredFromValues). plumbing writes it into every schema it
// infers; nothing stopped a chart author writing the same key into an authored values.schema.json,
// which would make the banner say "inferred" while the condition says Authored. So an AUTHORED
// schema has the root keyword removed here, at the one seam every consumer reads (the portal
// ConfigMap, CRD generation and its digest cache, the default-contradiction gate, SpecSchemaCurrent),
// and an inferred schema keeps it exactly as plumbing emits it.
//
// Only the ROOT keyword is the contract: valuesschema emits the marker at the root and nowhere else,
// so a key of that name under `properties` is an author's property (a value path) and is left alone.

// stripRootMarker returns schema without any root-level valuesschema.MarkerKey member. When the root
// carries none, the input is returned unchanged, byte for byte. Otherwise the root object is
// re-emitted with every other member's key and value bytes verbatim and in their original order (the
// portal relies on authoring order; json.Compact downstream removes whitespace only). A schema that
// is not a JSON object is returned unchanged: its consumers report it.
func stripRootMarker(schema []byte) ([]byte, error) {
	members, ok, err := rootMembers(schema)
	if err != nil || !ok {
		return schema, err
	}
	found := false
	for _, m := range members {
		if m.key == valuesschema.MarkerKey {
			found = true
			break
		}
	}
	if !found {
		return schema, nil
	}
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for _, m := range members {
		if m.key == valuesschema.MarkerKey {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(m.rawKey)
		out.WriteByte(':')
		out.Write(m.value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

type rootMember struct {
	key    string          // decoded key
	rawKey json.RawMessage // the key's JSON string exactly as written
	value  json.RawMessage // the value exactly as written
}

// rootMembers reads the members of a top-level JSON object in document order. ok is false when the
// document is not a single JSON object (so the caller leaves it alone); a syntax error inside an
// object is returned.
func rootMembers(doc []byte) ([]rootMember, bool, error) {
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := dec.Token(); err != nil { // '{'
		return nil, false, err
	}
	var members []rootMember
	for dec.More() {
		keyStart := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		key, isString := tok.(string)
		if !isString {
			return nil, false, fmt.Errorf("schema root: expected a member name, got %v", tok)
		}
		keyEnd := dec.InputOffset()
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false, err
		}
		members = append(members, rootMember{
			key:    key,
			rawKey: json.RawMessage(bytes.TrimLeft(trimmed[keyStart:keyEnd], " \t\r\n,")),
			value:  value,
		})
	}
	return members, true, nil
}
