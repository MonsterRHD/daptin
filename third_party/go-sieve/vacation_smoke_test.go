package sieve_test

import (
	"strings"
	"testing"

	sieve "github.com/foxcpp/go-sieve"
)

func TestVacationExtensionLoads(t *testing.T) {
	if _, err := sieve.Load(strings.NewReader(`require ["vacation"]; vacation "I am away";`),
		sieve.DefaultOptions()); err != nil {
		t.Fatalf("vacation script failed to load: %v", err)
	}
}

func TestVacationRequiresExtension(t *testing.T) {
	if _, err := sieve.Load(strings.NewReader(`vacation "away";`),
		sieve.DefaultOptions()); err == nil {
		t.Fatal("vacation without require was accepted")
	}
}
