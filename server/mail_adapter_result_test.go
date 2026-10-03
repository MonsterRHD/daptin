package server

import (
	"testing"

	"github.com/artpar/go-guerrilla/backends"
)

func TestTemporaryDeliveryResultUsesTransientCode(t *testing.T) {
	result := backends.NewResult(temporaryDeliveryResult)
	if result.Code() != 451 {
		t.Fatalf("SMTP code = %d, want 451 (temporary)", result.Code())
	}
}
