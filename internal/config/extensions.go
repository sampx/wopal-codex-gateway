package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// extensionFields keeps object keys this binary does not understand so a
// load/edit/save cycle cannot delete settings written by another version.
// Values stay as raw JSON, which also preserves large integer spellings.
type extensionFields map[string]json.RawMessage

// Clone returns an independent copy so callers cannot mutate the raw values of
// a loaded configuration through a shared map.
func (fields extensionFields) Clone() extensionFields {
	if len(fields) == 0 {
		return nil
	}
	clone := make(extensionFields, len(fields))
	for key, value := range fields {
		clone[key] = append(json.RawMessage(nil), value...)
	}
	return clone
}

func decodeExtensionFields(data []byte, known ...string) (extensionFields, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for key := range all {
		// encoding/json matches known field names case-insensitively. Keeping a
		// differently-cased spelling as an extension would re-emit it after the
		// canonical field, and the stale spelling would win on the next load.
		for _, name := range known {
			if strings.EqualFold(key, name) {
				delete(all, key)
				break
			}
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// marshalWithExtensions appends unknown keys after the typed fields so the
// familiar field order of newly written files stays stable.
func marshalWithExtensions(known any, extensions extensionFields) ([]byte, error) {
	payload, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}
	if len(extensions) == 0 {
		return payload, nil
	}
	keys := make([]string, 0, len(extensions))
	for key := range extensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	payload = bytes.TrimSpace(payload)
	if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
		return nil, fmt.Errorf("encode extension fields: object expected")
	}
	body := bytes.TrimSpace(payload[1 : len(payload)-1])
	result := bytes.NewBuffer(make([]byte, 0, len(payload)+64))
	result.WriteByte('{')
	result.Write(body)
	wroteField := len(body) > 0
	for _, key := range keys {
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		if wroteField {
			result.WriteByte(',')
		}
		result.Write(encodedKey)
		result.WriteByte(':')
		result.Write(extensions[key])
		wroteField = true
	}
	result.WriteByte('}')
	return result.Bytes(), nil
}
