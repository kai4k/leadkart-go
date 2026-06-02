package integrationevents

import "os"

// readBytes wraps os.ReadFile; _test.go keeps it out of the production binary.
func readBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
