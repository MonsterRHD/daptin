package scim

import (
	"strings"
	"testing"

	"github.com/doug-martin/goqu/v9"
)

func TestParseFilterValid(t *testing.T) {
	valid := []struct {
		name      string
		resources string
		input     string
		contains  []string
	}{
		{"userName eq", ResourceTypeUser, `userName eq "bjensen@example.com"`,
			[]string{"LOWER", "email", "bjensen@example.com"}},
		{"externalId eq", ResourceTypeUser, `externalId eq "ext-1"`,
			[]string{"external_id", "ext-1"}},
		{"displayName co", ResourceTypeGroup, `displayName co "eng"`,
			[]string{"LIKE", "%eng%"}},
		{"starts with", ResourceTypeUser, `displayName sw "Jo"`, []string{"jo%"}},
		{"ends with", ResourceTypeUser, `displayName ew "son"`, []string{"%son"}},
		{"not equal", ResourceTypeUser, `userName ne "a@b.c"`, []string{"<>"}},
		{"present", ResourceTypeUser, `externalId pr`, []string{"IS NOT NULL"}},
		{"and", ResourceTypeUser, `userName eq "a@b.c" and displayName eq "Alice"`,
			[]string{"email", " and "}},
		{"or with parentheses", ResourceTypeGroup,
			`(displayName eq "a") or (externalId eq "b")`, []string{" or "}},
		{"id eq", ResourceTypeUser, `id eq "00000000-0000-0000-0000-000000000001"`, []string{"reference_id"}},
		{"emails.value eq", ResourceTypeUser, `emails.value eq "x@y.z"`, []string{"email"}},
		{"escaped quote", ResourceTypeUser, `userName eq "a\"b@c.d"`, nil},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := ParseFilter(tc.resources, tc.input)
			if err != nil {
				t.Fatalf("expected valid filter, got %v", err)
			}
			sql, args, err := goqu.From("t").Where(expr).ToSQL()
			if err != nil {
				t.Fatalf("render sql: %v", err)
			}
			lowered := strings.ToLower(sql)
			for _, fragment := range tc.contains {
				if !strings.Contains(lowered, strings.ToLower(fragment)) {
					t.Fatalf("sql %q missing fragment %q", sql, fragment)
				}
			}
			if strings.Contains(tc.input, `\"`) {
				if !strings.Contains(sql, `a"b@c.d`) {
					t.Fatalf("escaped string not rendered, sql=%s args=%v", sql, args)
				}
			}
		})
	}

	t.Run("and with two supported attrs", func(t *testing.T) {
		expr, err := ParseFilter(ResourceTypeUser, `userName eq "a@b.c" and displayName eq "Alice"`)
		if err != nil {
			t.Fatal(err)
		}
		sql, _, _ := goqu.From("t").Where(expr).ToSQL()
		if !strings.Contains(strings.ToLower(sql), " and ") {
			t.Fatalf("expected AND predicate, got %s", sql)
		}
	})
}

func TestParseFilterInvalid(t *testing.T) {
	invalid := []struct {
		resources string
		input     string
	}{
		{ResourceTypeUser, `unknownAttr eq "x"`},
		{ResourceTypeGroup, `userName eq "x"`},
		{ResourceTypeUser, `userName bogus "x"`},
		{ResourceTypeUser, `(userName eq "x"`},
		{ResourceTypeUser, `userName eq`},
		{ResourceTypeUser, ``},
		{ResourceTypeUser, `id co "abc"`},
		{ResourceTypeUser, `id eq "not-a-uuid"`},
		{ResourceTypeGroup, `members eq "x"`},
		{ResourceTypeUser, `userName eq x@y.z`},
		{ResourceTypeUser, `userName eq "x" trailing`},
	}
	for _, tc := range invalid {
		t.Run(tc.input, func(t *testing.T) {
			if _, err := ParseFilter(tc.resources, tc.input); err == nil {
				t.Fatalf("expected invalid filter error for %q", tc.input)
			} else if _, ok := err.(*FilterError); !ok {
				t.Fatalf("expected *FilterError, got %T: %v", err, err)
			}
		})
	}
}

func TestETagRoundTrip(t *testing.T) {
	if ETag(7) != `W/"7"` {
		t.Fatalf("unexpected etag: %s", ETag(7))
	}
	version, err := ParseETag(`W/"7"`)
	if err != nil || version != 7 {
		t.Fatalf("parse weak etag: %v %v", version, err)
	}
	if _, err := ParseETag(`garbage`); err == nil {
		t.Fatal("expected invalid etag error")
	}
}
