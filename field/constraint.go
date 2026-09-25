package field

import (
	"fmt"
	"strconv"
	"strings"
)

// Constraints is the declarative validation carried on a model field, separate
// from the gombit policy tag. The generator writes it as a `validate` struct
// tag (semicolon-separated, so a pattern may contain commas). gombit generate
// and the admin meta reader both parse that tag.
type Constraints struct {
	Min       string
	Max       string
	MaxLength int
	Pattern   string
	Default   string
	// Enum is the allowed values for a string column. The GORM schema stores
	// an enum as a varchar, so this tag is how gombit generate recovers them.
	Enum []string
}

// FormatConstraints renders c as a validate tag value. Empty c is "".
func FormatConstraints(c Constraints) string {
	var parts []string
	if c.Min != "" {
		parts = append(parts, "min="+c.Min)
	}
	if c.Max != "" {
		parts = append(parts, "max="+c.Max)
	}
	if c.MaxLength > 0 {
		parts = append(parts, "max_length="+strconv.Itoa(c.MaxLength))
	}
	if c.Pattern != "" {
		parts = append(parts, "pattern="+c.Pattern)
	}
	if c.Default != "" {
		parts = append(parts, "default="+c.Default)
	}
	if len(c.Enum) > 0 {
		parts = append(parts, "enum="+strings.Join(c.Enum, ","))
	}
	return strings.Join(parts, ";")
}

// ParseConstraints parses a validate tag value. An empty tag is an empty
// Constraints. Unknown keys and empty values are errors.
func ParseConstraints(tag string) (Constraints, error) {
	var c Constraints
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return c, nil
	}
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			return c, fmt.Errorf("field: empty token in validate tag %q", tag)
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(val) == "" {
			return c, fmt.Errorf("field: validate token %q must be key=value", part)
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "min":
			c.Min = val
		case "max":
			c.Max = val
		case "max_length":
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("field: max_length %q must be a positive integer", val)
			}
			c.MaxLength = n
		case "pattern":
			c.Pattern = val
		case "default":
			c.Default = val
		case "enum":
			for _, v := range strings.Split(val, ",") {
				v = strings.TrimSpace(v)
				if v == "" {
					return c, fmt.Errorf("field: enum has an empty value")
				}
				c.Enum = append(c.Enum, v)
			}
		default:
			return c, fmt.Errorf("field: unknown validate key %q", key)
		}
	}
	return c, nil
}
