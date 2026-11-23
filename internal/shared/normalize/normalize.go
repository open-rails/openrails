package normalize

import "strings"

func Trim(value string) string {
	return strings.TrimSpace(value)
}

func Lower(value string) string {
	return strings.ToLower(Trim(value))
}

func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := Trim(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func OptionalString(value string) *string {
	if trimmed := Trim(value); trimmed != "" {
		return &trimmed
	}
	return nil
}

func FromPtr(value *string) string {
	if value == nil {
		return ""
	}
	return Trim(*value)
}

// Strings converts a slice between string types; nil stays nil.
func Strings[To, From ~string](in []From) []To {
	if in == nil {
		return nil
	}
	out := make([]To, len(in))
	for i, v := range in {
		out[i] = To(v)
	}
	return out
}
