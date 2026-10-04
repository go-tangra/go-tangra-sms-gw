//go:build !ui

package ui

import "testing"

func TestNoRemoteWithoutTag(t *testing.T) {
	if _, ok := Remote(); ok {
		t.Fatal("a backend-only build advertises a remote")
	}
}
