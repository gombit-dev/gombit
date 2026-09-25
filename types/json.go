package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
)

// JSON is a JSON object or array stored as text. Null is not a document:
// UnmarshalJSON and Schema reject it. Optional columns use NullJSON, because
// Huma validates anyOf before it honors a nullable tag, so one schema cannot
// both reject null (required) and allow it (optional).
type JSON json.RawMessage

// NullJSON is a JSON object, a JSON array, or null. Generated optional json
// columns use it so a stored NULL validates against the published contract.
type NullJSON JSON

// MarshalJSON returns null for a nil document and the raw document otherwise.
func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(j).MarshalJSON()
}

// MarshalJSON returns null for a nil document and the raw document otherwise.
func (j NullJSON) MarshalJSON() ([]byte, error) {
	return JSON(j).MarshalJSON()
}

// UnmarshalJSON stores a JSON object or array. Null is rejected.
func (j *JSON) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return fmt.Errorf("types: JSON must be an object or array")
	}
	parsed, err := parseJSONDocument(b)
	if err != nil {
		return err
	}
	*j = parsed
	return nil
}

// UnmarshalJSON stores a JSON object or array. Null clears the value.
func (j *NullJSON) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*j = nil
		return nil
	}
	parsed, err := parseJSONDocument(b)
	if err != nil {
		return err
	}
	*j = NullJSON(parsed)
	return nil
}

func parseJSONDocument(b []byte) (JSON, error) {
	if !json.Valid(b) {
		return nil, fmt.Errorf("types: invalid JSON")
	}
	var probe any
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, err
	}
	switch probe.(type) {
	case map[string]any, []any:
	default:
		return nil, fmt.Errorf("types: JSON must be an object or array")
	}
	return append(JSON(nil), b...), nil
}

// Value implements driver.Valuer. A nil document is SQL NULL.
func (j JSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return []byte(j), nil
}

// Value implements driver.Valuer. A nil document is SQL NULL.
func (j NullJSON) Value() (driver.Value, error) {
	return JSON(j).Value()
}

// Scan implements sql.Scanner.
func (j *JSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
		return nil
	case []byte:
		return j.UnmarshalJSON(append([]byte(nil), v...))
	case string:
		return j.UnmarshalJSON([]byte(v))
	default:
		return fmt.Errorf("types: cannot scan %T into JSON", src)
	}
}

// Scan implements sql.Scanner. SQL NULL clears the value.
func (j *NullJSON) Scan(src any) error {
	if src == nil {
		*j = nil
		return nil
	}
	var inner JSON
	if err := inner.Scan(src); err != nil {
		return err
	}
	*j = NullJSON(inner)
	return nil
}

// GormDataType tells GORM the column family when a model field omits an
// explicit type tag. Generated models also set gorm:"type:text".
func (JSON) GormDataType() string { return "text" }

// GormDataType tells GORM the column family when a model field omits an
// explicit type tag. Generated models also set gorm:"type:text".
func (NullJSON) GormDataType() string { return "text" }

// Schema implements huma.SchemaProvider. Null is not a member: a required
// field rejects JSON null. Optional fields use NullJSON.
func (JSON) Schema(huma.Registry) *huma.Schema {
	return jsonDocumentSchema(false)
}

// Schema implements huma.SchemaProvider. Object, array, and null are members.
func (NullJSON) Schema(huma.Registry) *huma.Schema {
	return jsonDocumentSchema(true)
}

func jsonDocumentSchema(allowNull bool) *huma.Schema {
	object := &huma.Schema{Type: huma.TypeObject}
	array := &huma.Schema{Type: huma.TypeArray, Items: &huma.Schema{}}
	if allowNull {
		// Huma returns early on a nullable schema before the type check, so a
		// null value matches these branches. It does not match a non-nullable
		// object or array schema. Putting Nullable on the parent anyOf is too
		// late: anyOf is validated first.
		object.Nullable = true
		array.Nullable = true
	}
	return &huma.Schema{AnyOf: []*huma.Schema{object, array}}
}
