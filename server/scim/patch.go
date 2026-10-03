package scim

import (
	"fmt"
	"strings"
)

// PathError is returned when a PATCH path cannot be resolved.
type PathError struct {
	kind string
	msg  string
}

func (e *PathError) Error() string { return e.msg }

// ScimType returns the SCIM error scimType (invalidPath or noTarget).
func (e *PathError) ScimType() string { return e.kind }

func newPathError(kind, format string, args ...interface{}) error {
	return &PathError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// AttrPath is a validated SCIM attribute path.
type AttrPath struct {
	// Attr is the canonical top-level attribute (userName, name, emails,
	// members, ...).
	Attr string
	// SubAttr is the optional sub-attribute (formatted, value, primary, ...).
	SubAttr string
	// HasFilter/FilterValue carry the members[value eq "..."] selector.
	HasFilter   bool
	FilterValue string
}

var userSubAttributes = map[string]map[string]bool{
	"name":   {"formatted": true, "familyName": true, "givenName": true, "middleName": true},
	"emails": {"value": true, "type": true, "primary": true, "display": true},
}

var userTopAttributes = map[string]bool{
	"userName": true, "externalId": true, "displayName": true,
	"active": true, "password": true, "name": true, "emails": true,
}

var groupTopAttributes = map[string]bool{
	"displayName": true, "externalId": true, "members": true,
}

// ParsePath validates a SCIM PATCH path for the given resource type.
func ParsePath(resourceType, path string) (*AttrPath, error) {
	normalized := strings.TrimSpace(path)
	if normalized == "" {
		return &AttrPath{}, nil
	}

	openBracket := strings.Index(normalized, "[")
	head := normalized
	var selector string
	if openBracket >= 0 {
		if !strings.HasSuffix(normalized, "]") || strings.LastIndex(normalized, "[") != openBracket {
			return nil, newPathError(ScimTypeInvalidPath, "malformed attribute filter in path %q", path)
		}
		head = normalized[:openBracket]
		selector = normalized[openBracket+1 : len(normalized)-1]
	}

	parts := strings.Split(head, ".")
	attr := parts[0]
	subAttr := ""
	if len(parts) == 2 {
		subAttr = parts[1]
	} else if len(parts) > 2 {
		return nil, newPathError(ScimTypeInvalidPath, "unsupported deep path %q", path)
	}

	topAttributes := userTopAttributes
	if resourceType == ResourceTypeGroup {
		topAttributes = groupTopAttributes
	}
	if resourceType == ResourceTypeUser && attr == "members" {
		return nil, newPathError(ScimTypeInvalidPath, "members can only be patched on Group resources")
	}
	if !topAttributes[attr] {
		return nil, newPathError(ScimTypeNoTarget, "unknown attribute %q", attr)
	}

	parsed := &AttrPath{Attr: attr, SubAttr: subAttr}

	if resourceType == ResourceTypeUser && subAttr != "" {
		allowed, ok := userSubAttributes[attr]
		if !ok || !allowed[subAttr] {
			return nil, newPathError(ScimTypeNoTarget, "unknown sub-attribute %q", path)
		}
	}

	if selector != "" {
		if resourceType != ResourceTypeGroup || attr != "members" {
			return nil, newPathError(ScimTypeInvalidPath, "attribute filters are only supported for group members")
		}
		value, err := parseMemberValueSelector(selector)
		if err != nil {
			return nil, err
		}
		parsed.HasFilter = true
		parsed.FilterValue = value
	}
	return parsed, nil
}

// parseMemberValueSelector accepts: value eq "uuid"
func parseMemberValueSelector(selector string) (string, error) {
	tokens, err := tokenizeFilter(strings.TrimSpace(selector))
	if err != nil {
		return "", newPathError(ScimTypeInvalidPath, "invalid member selector: %v", err)
	}
	if len(tokens) != 3 {
		return "", newPathError(ScimTypeInvalidPath, "member selector must be %q", `value eq "<id>"`)
	}
	if tokens[0].kind != tokWord || strings.ToLower(tokens[0].value) != "value" {
		return "", newPathError(ScimTypeInvalidPath, "member selector must filter on value")
	}
	if tokens[1].kind != tokOp || strings.ToLower(tokens[1].value) != "eq" {
		return "", newPathError(ScimTypeInvalidPath, "member selector only supports eq")
	}
	if tokens[2].kind != tokString || tokens[2].value == "" {
		return "", newPathError(ScimTypeInvalidPath, "member selector value must be a quoted non-empty string")
	}
	return tokens[2].value, nil
}
