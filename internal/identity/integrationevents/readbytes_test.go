package integrationevents

import "os"

// readBytes is a thin os.ReadFile wrapper. Lives in a separate
// _test.go file so it isn't compiled into the production binary.
func readBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
