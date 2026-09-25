//go:build !darwin && !linux

package main

import "testing"

func processGroupsSupported() bool { return false }

func assertDescendantStoppedFromFile(t *testing.T, _ string) {
	t.Helper()
	t.Skip("owned process-group cleanup is supported only on Darwin and Linux")
}

func cleanupDescendantFromFile(string) {}
