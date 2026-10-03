package resource

import (
	"fmt"
	"strings"

	sieve "github.com/foxcpp/go-sieve"
)

// supportedSieveCapabilities is the single authority for both ManageSieve
// SIEVE capability announcements and script upload/activation validation.
var supportedSieveCapabilities = []string{
	"fileinto",
	"envelope",
	"reject",
	"ereject",
	"imap4flags",
	"variables",
	"relational",
	"copy",
	"subaddress",
	"environment",
	"body",
	"vacation",
}

// SupportedSieveCapabilities returns the Sieve language extensions accepted
// by this server.
func SupportedSieveCapabilities() []string {
	return append([]string(nil), supportedSieveCapabilities...)
}

// ValidateSieveScript performs a complete parse of a Sieve script so that
// syntax errors and unsupported capabilities are rejected before a script can
// be stored active. Parse errors carry line and column positions.
func ValidateSieveScript(content string) error {
	if _, err := sieve.Load(strings.NewReader(content), sieve.DefaultOptions()); err != nil {
		return fmt.Errorf("sieve validation failed: %w", err)
	}
	return nil
}
