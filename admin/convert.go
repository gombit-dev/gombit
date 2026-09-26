package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/types"
)

// patternRejectsEmpty reports that the pattern does not match "".
func patternRejectsEmpty(pattern string) bool {
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return true
	}
	return !re.MatchString("")
}

// constraintMessage reports a min, max, max_length, or pattern failure for a
// submitted value. An absent value (nil) is not a constraint failure; the
// caller applies a default or the required check.
func constraintMessage(f Field, raw any) string {
	if raw == nil {
		return ""
	}
	if f.MaxLength > 0 {
		if s, ok := raw.(string); ok && utf8.RuneCountInString(s) > f.MaxLength {
			return "is too long"
		}
	}
	if s, ok := raw.(string); ok {
		if msg := formatMessage(f.Format, s); msg != "" {
			return msg
		}
	}
	if f.Pattern != "" {
		if s, ok := raw.(string); ok {
			re, err := regexp.Compile(f.Pattern)
			if err != nil || !re.MatchString(s) {
				return "does not match pattern"
			}
		}
	}
	if s, ok := raw.(string); ok {
		if msg := choiceMessage(f, s); msg != "" {
			return msg
		}
	}
	if f.Minimum == "" && f.Maximum == "" {
		return ""
	}
	got, ok := decimalValue(raw)
	if !ok {
		return ""
	}
	if f.Minimum != "" {
		bound, err := decimal.NewFromString(f.Minimum)
		if err != nil || got.LessThan(bound) {
			return "must be at least " + f.Minimum
		}
	}
	if f.Maximum != "" {
		bound, err := decimal.NewFromString(f.Maximum)
		if err != nil || got.GreaterThan(bound) {
			return "must be at most " + f.Maximum
		}
	}
	return ""
}

func choiceMessage(f Field, s string) string {
	if len(f.Choices) == 0 || s == "" {
		return ""
	}
	labels := make([]string, 0, len(f.Choices))
	for _, c := range f.Choices {
		if c.Value == s {
			return ""
		}
		if c.Label != "" {
			labels = append(labels, c.Label)
		} else {
			labels = append(labels, c.Value)
		}
	}
	return "must be one of " + strings.Join(labels, ", ")
}

// formatMessage applies the model format tag the same way Huma's
// validateFormat does. blankToNull uses formatMessage(format, "").
func formatMessage(format, s string) string {
	if !field.FormatRejects(format, s) {
		return ""
	}
	switch format {
	case "email", "idn-email":
		return "must be an email address"
	case "uri", "iri":
		return "must be an absolute URI"
	case "ip", "ipv4", "ipv6":
		return "must be an IP address"
	default:
		return "must match format " + format
	}
}

func decimalValue(raw any) (decimal.Decimal, bool) {
	switch v := raw.(type) {
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(v))
		return d, err == nil
	case float64:
		return decimal.NewFromFloat(v), true
	case int:
		return decimal.NewFromInt(int64(v)), true
	case int64:
		return decimal.NewFromInt(v), true
	case json.Number:
		d, err := decimal.NewFromString(v.String())
		return d, err == nil
	default:
		return decimal.Decimal{}, false
	}
}

func coerceValue(raw any, ft FieldType) (any, error) {
	if raw == nil {
		return nil, nil
	}
	switch ft {
	case TypeString, TypeText, TypeUUID:
		s, err := asString(raw)
		if err != nil {
			return nil, err
		}
		if ft == TypeUUID && !validUUID(s) {
			return nil, fmt.Errorf("must be a UUID")
		}
		return s, nil
	case TypeInteger:
		return asInt64(raw)
	case TypeFloat:
		return asFloat64(raw)
	case TypeDecimal:
		// Keep the exact textual value — coercing money through float64 would
		// lose precision. The setter feeds it to decimal.UnmarshalText.
		return asDecimalString(raw)
	case TypeBoolean:
		return asBool(raw)
	case TypeDateTime:
		return asDateTime(raw)
	case TypeDate:
		return asDate(raw)
	case TypeTime:
		s, err := asString(raw)
		if err != nil {
			return nil, err
		}
		clock, err := types.ParseTimeOfDay(s)
		if err != nil {
			return nil, fmt.Errorf("must be a time of day (HH:MM:SS)")
		}
		return clock.String(), nil
	case TypeDuration:
		s, err := asString(raw)
		if err != nil {
			return nil, err
		}
		span, err := types.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("must be a duration (for example 1h30m)")
		}
		return span.String(), nil
	case TypeJSON:
		return raw, nil
	case TypeRelation:
		// belongs_to stores the FK as integer or string, whichever the
		// payload used. The setter converts to the Go field type.
		if s, err := asString(raw); err == nil {
			if n, nerr := strconv.ParseInt(s, 10, 64); nerr == nil {
				return n, nil
			}
			return s, nil
		}
		return asInt64(raw)
	default:
		return nil, fmt.Errorf("unsupported field type %q", ft)
	}
}

func coerceFilter(raw string, ft FieldType) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	return coerceValue(raw, ft)
}

func coercePathID(id string, ft FieldType) (any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("missing id")
	}
	if ft == "" {
		ft = TypeString
	}
	return coerceValue(id, ft)
}

func asString(raw any) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		return "", fmt.Errorf("must be a string")
	}
}

// asDecimalString returns raw as an exact decimal string. A JSON float64 is
// rejected: JSON bodies decode numbers to float64, which cannot represent a
// decimal exactly, so a decimal must be sent as a JSON string (or json.Number)
// to preserve precision. This is why the admin SPA submits decimals as strings.
func asDecimalString(raw any) (string, error) {
	var s string
	switch v := raw.(type) {
	case json.Number:
		s = v.String()
	case string:
		s = strings.TrimSpace(v)
	case fmt.Stringer:
		s = v.String()
	case float64, float32:
		return "", fmt.Errorf("must be a decimal string, not a JSON number (float precision)")
	default:
		return "", fmt.Errorf("must be a decimal")
	}
	if _, err := decimal.NewFromString(s); err != nil {
		return "", fmt.Errorf("must be a decimal")
	}
	return s, nil
}

func asInt64(raw any) (int64, error) {
	switch v := raw.(type) {
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, fmt.Errorf("must be an integer")
		}
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint64:
		if v > math.MaxInt64 {
			return 0, fmt.Errorf("must be an integer")
		}
		return int64(v), nil
	case float32:
		if float32(int64(v)) != v {
			return 0, fmt.Errorf("must be an integer")
		}
		return int64(v), nil
	case float64:
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("must be an integer")
		}
		return int64(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return n, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return n, nil
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}

func asFloat64(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case json.Number:
		n, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("must be a number")
		}
		return n, nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("must be a number")
		}
		return n, nil
	default:
		if n, err := asInt64(raw); err == nil {
			return float64(n), nil
		}
		return 0, fmt.Errorf("must be a number")
	}
}

func asBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		default:
			return false, fmt.Errorf("must be a boolean")
		}
	default:
		return false, fmt.Errorf("must be a boolean")
	}
}

func asDateTime(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case time.Time:
		return v, nil
	case string:
		s := strings.TrimSpace(v)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("must be an RFC3339 datetime")
	default:
		return time.Time{}, fmt.Errorf("must be an RFC3339 datetime")
	}
}

func asDate(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case time.Time:
		return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC), nil
	case string:
		s := strings.TrimSpace(v)
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return t, nil
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
		}
		return time.Time{}, fmt.Errorf("must be a date (YYYY-MM-DD)")
	default:
		return time.Time{}, fmt.Errorf("must be a date (YYYY-MM-DD)")
	}
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
