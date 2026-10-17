package contract

import (
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

var requiredPropertyRE = regexp.MustCompile(`(?i)^expected required property (\S+) to be present$`)

// FieldsFromErrors maps Huma validation detail errors into D10 field errors.
//
// Body locations lose the leading "body." prefix so JSON field names match the
// request body. Query, path, and header locations keep their prefixes. A
// missing required property is reported against the object that lacks it, so
// the property named in its message is what keys the error: `title` at the top
// level, `address.city` under a nested body object.
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
// header locations keep theirs. A missing required property is reported against
// the object that lacks it, with the property only in the message, so the
// message's property names the field: alone it is the key, and under a nested
// body location it grows that path into `address.city` / `items[0].name`, the
// shape every other nested body error already has and the field path a form
// binds. Only a body location names such a field, so a query, path or header
// key stays as it is.
func fieldKey(location, message string) string {
	location = strings.TrimSpace(location)
	nested := strings.HasPrefix(location, "body.")
	key := location
	if nested {
		key = strings.TrimPrefix(location, "body.")
	} else if location == "body" || location == "" {
		key = "_error"
	}
	prop := fieldFromMessage(message)
	if prop == "" {
		return key
	}
	if key == "_error" {
		return prop
	}
	if nested {
		return key + "." + prop
	}
	return key
}

func fieldFromMessage(message string) string {
	matches := requiredPropertyRE.FindStringSubmatch(strings.TrimSpace(message))
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}
