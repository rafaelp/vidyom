package persistence

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// jsonArg encodes a value for a nullable JSON TEXT column. A nil pointer or nil raw
// message is NULL.
func jsonArg(v any) (driver.Value, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		if len(t) == 0 {
			return nil, nil
		}
		return string(t), nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode JSON column: %w", err)
	}
	if string(data) == "null" {
		return nil, nil
	}
	return string(data), nil
}

// jsonList encodes a slice for a NOT NULL JSON TEXT column, nil becoming "[]".
func jsonList[T any](items []T) (string, error) {
	if items == nil {
		return "[]", nil
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("encode JSON column: %w", err)
	}
	return string(data), nil
}

// jsonCol decodes a JSON TEXT column into dst. NULL and an empty string leave dst
// untouched.
type jsonCol struct{ dst any }

func (j jsonCol) Scan(src any) error {
	var data []byte
	switch v := src.(type) {
	case nil:
		return nil
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		return fmt.Errorf("unexpected JSON column type %T", src)
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, j.dst)
}

// rawCol scans a nullable TEXT column into a json.RawMessage, preserving its bytes.
type rawCol struct{ dst *json.RawMessage }

func (r rawCol) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*r.dst = nil
	case string:
		*r.dst = json.RawMessage(v)
	case []byte:
		*r.dst = append(json.RawMessage(nil), v...)
	default:
		return fmt.Errorf("unexpected JSON column type %T", src)
	}
	return nil
}

// boolInt renders a Go bool for an INTEGER column.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
