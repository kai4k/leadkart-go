package main

import "testing"

// Every sub-command that applies goose migrations forward must also run River's
// migrations, or it leaves a database with the application schema and no job
// tables; the ones that roll back or only inspect must not.
func TestMigratesRiver(t *testing.T) {
	t.Parallel()
	for command, want := range map[string]bool{
		"up": true, "up-by-one": true, "up-to": true, "redo": true,
		"down": false, "down-to": false, "reset": false,
		"status": false, "version": false, "validate": false, "fix": false, "create": false,
	} {
		if got := migratesRiver(command); got != want {
			t.Errorf("migratesRiver(%q) = %v, want %v", command, got, want)
		}
	}
}
