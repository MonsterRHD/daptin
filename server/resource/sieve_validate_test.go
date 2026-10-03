package resource

import (
	"strings"
	"testing"
)

func TestValidateSieveScriptAcceptsValidScripts(t *testing.T) {
	scripts := []string{
		`#empty script`,
		`require ["fileinto"]; fileinto "INBOX";`,
		`require ["vacation"]; if header :contains "X-Test" ["yes"] { vacation "away"; stop; }`,
		`if address :all :is "from" "a@example.test" { keep; }`,
		`if size :over 1000 { discard; }`,
		`require ["body", "fileinto"]; if body :contains "secret" { fileinto "Secret"; }`,
	}
	for index, script := range scripts {
		if err := ValidateSieveScript(script); err != nil {
			t.Fatalf("script %d rejected: %v", index, err)
		}
	}
}

func TestValidateSieveScriptRejectsSyntaxErrorsWithPosition(t *testing.T) {
	err := ValidateSieveScript(`keep(;`)
	if err == nil {
		t.Fatal("syntax error script accepted")
	}
	message := err.Error()
	if !strings.Contains(message, ":") {
		t.Fatalf("error missing line:column position: %v", err)
	}
}

func TestValidateSieveScriptRejectsUnknownCapability(t *testing.T) {
	if err := ValidateSieveScript(`require ["nonexistent-extension"]; keep;`); err == nil {
		t.Fatal("unknown capability accepted")
	}
}

func TestAdvertisedCapabilitiesMatchInterpreter(t *testing.T) {
	for _, capability := range SupportedSieveCapabilities() {
		probe := `require ["` + capability + `"]; keep;`
		if err := ValidateSieveScript(probe); err != nil {
			t.Errorf("advertised capability %q rejected by interpreter: %v", capability, err)
		}
	}

	if err := ValidateSieveScript(`require ["definitely-not-supported"]; keep;`); err == nil {
		t.Error("non-advertised capability accepted")
	}
}
