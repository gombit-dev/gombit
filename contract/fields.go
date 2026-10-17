package contract

import (
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

var (
	requiredPropertyRE          = regexp.MustCompile(`(?i)^expected required property (\S+) to be present$`)
	dependentRequiredPropertyRE = regexp.MustCompile(`(?i)^expected property (\S+) to be present when \S+ is present$`)
)

// FieldsFromErrors maps Huma validation detail errors into D10 field errors.
//
// Body locations lose the leading "body." prefix so JSON field names match the
// request body. Query, path, and header locations keep their prefixes. A
// missing property — required, or pulled in by `dependentRequired` — is
// reported against the object that lacks it, whatever kind of location that
// object sits at, so the property named in the message is what the key grows
// with: `title` at the request root, `address.city` under a nested body
// object, `body[1].sku` under a top-level array body, `query.filter.kind`
// under a deepObject query param.
func FieldsFromErrors(errs ...error) map[string][]string {
	if len(errs) == 0 {
		return nil
	}

	fields := make(map[string][]string)
	for _, err := range errs {
		if err == nil {
			continue
		}
		key, message := fieldFromError(err)
		if key == "" || message == "" {
			continue
		}
		fields[key] = append(fields[key], message)
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func fieldFromError(err error) (key, message string) {
	if detailer, ok := err.(huma.ErrorDetailer); ok {
		detail := detailer.ErrorDetail()
		if detail == nil {
			return "", ""
		}
		message = strings.TrimSpace(detail.Message)
		if message == "" {
			message = strings.TrimSpace(detail.Error())
		}
		key = fieldKey(detail.Location, message)
		return key, message
	}

	message = strings.TrimSpace(err.Error())
	if message == "" {
		return "", ""
	}
	if inferred := fieldFromMessage(message); inferred != "" {
		return inferred, message
	}
	// Generic errors are not field-level validation details. Huma's
	// unexpected-500 path wraps the handler error here; putting it in
	// fields._error would classify the response as validation_error.
	return "", ""
}

// fieldKey maps a Huma validation location to a D10 field key. Body locations
// lose the "body." prefix so keys name request-body fields; query, path and
// header locations keep theirs. Both of Huma's missing-property messages —
// `expected required property %s to be present` and `expected property %s to
// be present when %s is present` (dependentRequired) — are added at the path
// of the object that lacks the property, and that object can sit anywhere:
// a nested body object, a top-level array-body item (`body[1]`, where Huma's
// PushIndex appends `[i]` without a dot), a deepObject query param, or a form
// object. So the property named in the message grows the location's key into
// the full field path: `title` alone at the request root, `address.city` under
// a nested body object, `body[1].sku`, `query.filter.kind`. Every other
// message keeps the location's key as it is.
func fieldKey(location, message string) string {
	location = strings.TrimSpace(location)
	key := strings.TrimPrefix(location, "body.")
	if location == "" || location == "body" {
		key = "_error"
	}
	prop := fieldFromMessage(message)
	if prop == "" {
		return key
	}
	if key == "_error" {
		return prop
	}
	return key + "." + prop
}

func fieldFromMessage(message string) string {
	message = strings.TrimSpace(message)
	for _, re := range []*regexp.Regexp{requiredPropertyRE, dependentRequiredPropertyRE} {
		if matches := re.FindStringSubmatch(message); len(matches) == 2 {
			return matches[1]
		}
	}
	return ""
}
