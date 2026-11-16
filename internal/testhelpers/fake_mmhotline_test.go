package testhelpers_test

import (
	"testing"

	"mmwatchdog/internal/testhelpers"
)

func TestDecodeMMHotlineCheckInRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	body := []byte(`{"sent_at":"2026-10-16T12:00:00Z","sent_by_node_name":"app-1"}`)
	if _, err := testhelpers.DecodeMMHotlineCheckIn(body); err != nil {
		t.Fatal(err)
	}
	if _, err := testhelpers.DecodeMMHotlineCheckIn(append(body, []byte(`{}`)...)); err == nil {
		t.Fatal("accepted a check-in followed by another JSON value")
	}
}
