package metadata

import (
	"encoding/json"
	"io"
)

// WriteJSON encodes the metadata as pretty-printed JSON (results/latest/
// metadata.json).
func WriteJSON(w io.Writer, m Metadata) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// ReadJSON decodes a metadata.json. It is the inverse of WriteJSON, used by the
// producers that merge into an existing snapshot rather than replacing it.
func ReadJSON(r io.Reader) (Metadata, error) {
	var m Metadata
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Metadata{}, err
	}
	return m, nil
}
