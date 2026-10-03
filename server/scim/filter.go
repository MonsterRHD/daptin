package scim

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/doug-martin/goqu/v9"
	"github.com/google/uuid"
)

// FilterError marks a malformed or unsupported SCIM filter.
type FilterError struct{ msg string }

func (e *FilterError) Error() string { return e.msg }

func newFilterError(format string, args ...interface{}) error {
	return &FilterError{msg: fmt.Sprintf(format, args...)}
}

type filterNode interface{ isFilterNode() }

type filterOr struct{ children []filterNode }
type filterAnd struct{ children []filterNode }
type filterComparison struct {
	attr  string
	op    string
	value interface{} // nil for "pr"
}

func (*filterOr) isFilterNode()         {}
func (*filterAnd) isFilterNode()        {}
func (*filterComparison) isFilterNode() {}

// supported filterable attributes: SCIM name -> physical Daptin column.
var userFilterColumns = map[string]string{
	"id":           "reference_id",
	"externalid":   "external_id",
	"username":     "email",
	"displayname":  "name",
	"emails":       "email",
	"emails.value": "email",
}

var groupFilterColumns = map[string]string{
	"id":          "reference_id",
	"externalid":  "external_id",
	"displayname": "name",
}

var stringOperators = map[string]bool{
	"eq": true, "ne": true, "co": true, "sw": true, "ew": true,
	"gt": true, "ge": true, "lt": true, "le": true,
}

// ParseFilter parses a RFC 7643 §3.4.2.2 filter expression and converts it to
// a goqu predicate over physical Daptin columns.
func ParseFilter(resourceType, input string) (goqu.Expression, error) {
	node, err := parseFilterExpression(strings.TrimSpace(input))
	if err != nil {
		return nil, err
	}
	columns := userFilterColumns
	if resourceType == ResourceTypeGroup {
		columns = groupFilterColumns
	}
	return nodeToGoqu(node, columns)
}

// ---- scanner ----

type filterTokenKind int

const (
	tokLParen filterTokenKind = iota
	tokRParen
	tokWord
	tokString
	tokAnd
	tokOr
	tokOp
	tokTrue
	tokFalse
	tokNull
)

type filterToken struct {
	kind  filterTokenKind
	value string
}

func tokenizeFilter(input string) ([]filterToken, error) {
	tokens := make([]filterToken, 0)
	i := 0
	for i < len(input) {
		switch {
		case input[i] == ' ' || input[i] == '\t':
			i++
		case input[i] == '(':
			tokens = append(tokens, filterToken{kind: tokLParen})
			i++
		case input[i] == ')':
			tokens = append(tokens, filterToken{kind: tokRParen})
			i++
		case input[i] == '"':
			value, next, err := scanFilterString(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, filterToken{kind: tokString, value: value})
			i = next
		default:
			start := i
			for i < len(input) && input[i] != ' ' && input[i] != '\t' && input[i] != '(' && input[i] != ')' && input[i] != '"' {
				i++
			}
			word := input[start:i]
			tokens = append(tokens, filterToken{kind: classifyFilterWord(word), value: word})
		}
	}
	return tokens, nil
}

func scanFilterString(input string, openQuote int) (string, int, error) {
	var builder strings.Builder
	i := openQuote + 1
	for i < len(input) {
		if input[i] == '\\' && i+1 < len(input) {
			switch input[i+1] {
			case '"':
				builder.WriteByte('"')
			case '\\':
				builder.WriteByte('\\')
			case 'n':
				builder.WriteByte('\n')
			case 't':
				builder.WriteByte('\t')
			case 'f':
				builder.WriteByte('\f')
			case 'r':
				builder.WriteByte('\r')
			default:
				builder.WriteByte(input[i+1])
			}
			i += 2
			continue
		}
		if input[i] == '"' {
			return builder.String(), i + 1, nil
		}
		builder.WriteByte(input[i])
		i++
	}
	return "", i, newFilterError("unterminated string literal")
}

func classifyFilterWord(word string) filterTokenKind {
	switch strings.ToLower(word) {
	case "and":
		return tokAnd
	case "or":
		return tokOr
	case "pr":
		return tokOp
	case "eq", "ne", "co", "sw", "ew", "gt", "ge", "lt", "le":
		return tokOp
	case "true":
		return tokTrue
	case "false":
		return tokFalse
	case "null":
		return tokNull
	default:
		return tokWord
	}
}

// ---- parser ----

type filterParser struct {
	tokens []filterToken
	pos    int
}

func parseFilterExpression(input string) (filterNode, error) {
	if input == "" {
		return nil, newFilterError("empty filter")
	}
	tokens, err := tokenizeFilter(input)
	if err != nil {
		return nil, err
	}
	p := &filterParser{tokens: tokens}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.tokens) {
		return nil, newFilterError("unexpected trailing input near %q", p.tokens[p.pos].value)
	}
	return node, nil
}

func (p *filterParser) parseOr() (filterNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	children := []filterNode{left}
	for p.peekKind() == tokOr {
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	if len(children) == 1 {
		return children[0], nil
	}
	return &filterOr{children: children}, nil
}

func (p *filterParser) parseAnd() (filterNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	children := []filterNode{left}
	for p.peekKind() == tokAnd {
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	if len(children) == 1 {
		return children[0], nil
	}
	return &filterAnd{children: children}, nil
}

func (p *filterParser) parseUnary() (filterNode, error) {
	if p.peekKind() == tokLParen {
		p.pos++
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peekKind() != tokRParen {
			return nil, newFilterError("missing closing parenthesis")
		}
		p.pos++
		return node, nil
	}
	return p.parseComparison()
}

func (p *filterParser) parseComparison() (filterNode, error) {
	attrToken := p.accept(tokWord)
	if attrToken == nil {
		return nil, newFilterError("expected attribute path")
	}
	opToken := p.accept(tokOp)
	if opToken == nil {
		return nil, newFilterError("expected comparison operator after %q", attrToken.value)
	}
	op := strings.ToLower(opToken.value)
	if op == "pr" {
		return &filterComparison{attr: strings.ToLower(attrToken.value), op: op, value: nil}, nil
	}
	valueToken := p.next()
	if valueToken == nil {
		return nil, newFilterError("operator %q requires a value", op)
	}
	var value interface{}
	switch valueToken.kind {
	case tokString:
		value = valueToken.value
	case tokTrue:
		value = true
	case tokFalse:
		value = false
	case tokNull:
		value = nil
	case tokWord:
		// Unquoted token: only numbers are accepted as comparison values.
		number, err := parseFilterNumber(valueToken.value)
		if err != nil {
			return nil, newFilterError("unsupported unquoted value %q; quote string values", valueToken.value)
		}
		value = number
	default:
		return nil, newFilterError("invalid comparison value for operator %q", op)
	}
	return &filterComparison{attr: strings.ToLower(attrToken.value), op: op, value: value}, nil
}

func parseFilterNumber(word string) (interface{}, error) {
	if integer, err := strconv.ParseInt(word, 10, 64); err == nil {
		return integer, nil
	}
	if number, err := strconv.ParseFloat(word, 64); err == nil {
		return number, nil
	}
	return nil, fmt.Errorf("not a number: %s", word)
}

func (p *filterParser) next() *filterToken {
	if p.pos >= len(p.tokens) {
		return nil
	}
	t := p.tokens[p.pos]
	p.pos++
	return &t
}

func (p *filterParser) accept(kind filterTokenKind) *filterToken {
	if p.peekKind() == kind {
		t := p.tokens[p.pos]
		p.pos++
		return &t
	}
	return nil
}

func (p *filterParser) peekKind() filterTokenKind {
	if p.pos >= len(p.tokens) {
		return -1
	}
	return p.tokens[p.pos].kind
}

// ---- goqu conversion ----

func nodeToGoqu(node filterNode, columns map[string]string) (goqu.Expression, error) {
	switch n := node.(type) {
	case *filterOr:
		parts := make([]goqu.Expression, 0, len(n.children))
		for _, child := range n.children {
			part, err := nodeToGoqu(child, columns)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		return goqu.Or(parts...), nil
	case *filterAnd:
		parts := make([]goqu.Expression, 0, len(n.children))
		for _, child := range n.children {
			part, err := nodeToGoqu(child, columns)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		return goqu.And(parts...), nil
	case *filterComparison:
		return comparisonToGoqu(n, columns)
	default:
		return nil, newFilterError("invalid filter expression")
	}
}

func comparisonToGoqu(c *filterComparison, columns map[string]string) (goqu.Expression, error) {
	column, ok := columns[c.attr]
	if !ok {
		return nil, newFilterError("unknown or unsupported filter attribute %q", c.attr)
	}
	if c.op == "pr" {
		if column == "reference_id" {
			return nil, newFilterError("operator pr is not supported on attribute %q", c.attr)
		}
		return goqu.And(goqu.I(column).IsNotNull(), goqu.I(column).Neq("")), nil
	}
	if column == "reference_id" {
		if c.op != "eq" && c.op != "ne" {
			return nil, newFilterError("operator %q is not supported on attribute %q", c.op, c.attr)
		}
		stringValue, ok := c.value.(string)
		if !ok {
			return nil, newFilterError("attribute %q requires a UUID string value", c.attr)
		}
		referenceIDBytes, err := parseReferenceIDBytes(stringValue)
		if err != nil {
			return nil, newFilterError("attribute %q requires a valid UUID value", c.attr)
		}
		if c.op == "eq" {
			return goqu.Ex{column: referenceIDBytes}, nil
		}
		return goqu.Ex{column: goqu.Op{"neq": referenceIDBytes}}, nil
	}
	stringValue, ok := c.value.(string)
	if !ok {
		return nil, newFilterError("attribute %q requires a string value", c.attr)
	}
	columnExpr := goqu.L("LOWER(?)", goqu.I(column))
	switch c.op {
	case "eq":
		return goqu.L("? = LOWER(?)", columnExpr, stringValue), nil
	case "ne":
		return goqu.L("? <> LOWER(?)", columnExpr, stringValue), nil
	case "co":
		return likeExpression(columnExpr, "%"+escapeLike(stringValue)+"%"), nil
	case "sw":
		return likeExpression(columnExpr, escapeLike(stringValue)+"%"), nil
	case "ew":
		return likeExpression(columnExpr, "%"+escapeLike(stringValue)), nil
	case "gt", "ge", "lt", "le":
		sqlOperator := map[string]string{"gt": ">", "ge": ">=", "lt": "<", "le": "<="}[c.op]
		return goqu.L(fmt.Sprintf("? %s LOWER(?)", sqlOperator), columnExpr, stringValue), nil
	default:
		return nil, newFilterError("operator %q is not supported", c.op)
	}
}

func likeExpression(column goqu.Expression, pattern string) goqu.Expression {
	return goqu.L("? LIKE LOWER(?) ESCAPE '\\'", column, pattern)
}

func parseReferenceIDBytes(value string) ([]byte, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, err
	}
	return parsed[:], nil
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(strings.ToLower(value))
}
