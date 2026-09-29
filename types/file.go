package types

// File is a storage-backed field's value: the key of an object in the
// application's storage (framework.App.Storage()), never the file's bytes.
// The column holds the key as a string; an empty File is no file. The
// generated resource code accepts a key only for an upload that passed the
// field's policy, returns a file object (key, filename, size, content
// type, URL) on reads, and stores one record per file (a unique index), so
// the record owns it. See docs/fields.md.
type File string

// Key returns the object key ("" for no file).
func (f File) Key() string { return string(f) }

// IsZero reports whether there is no file.
func (f File) IsZero() bool { return f == "" }

// Image is a File whose field accepts images only (detected from their
// bytes); everything else is as for File.
type Image string

// Key returns the object key ("" for no image).
func (i Image) Key() string { return string(i) }

// IsZero reports whether there is no image.
func (i Image) IsZero() bool { return i == "" }
