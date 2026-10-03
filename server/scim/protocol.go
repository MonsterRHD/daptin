package scim

import (
	"errors"
	"strconv"
	"strings"
)

// ETag renders the weak entity tag for a Daptin resource row version.
func ETag(version int64) string {
	return "W/\"" + strconv.FormatInt(version, 10) + "\""
}

// ParseETag extracts the numeric version contained in an If-Match value.
// Both W/"3" and the bare quoted form are accepted.
func ParseETag(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "W/") {
		trimmed = strings.TrimSpace(trimmed[2:])
	}
	if len(trimmed) < 2 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		return 0, errors.New("invalid etag")
	}
	return strconv.ParseInt(trimmed[1:len(trimmed)-1], 10, 64)
}

// BasePath is the SCIM service root.
const BasePath = "/scim/v2"
