// Package testenv provides environment predicates shared by tests.
// It is test support only and must not be imported by production code.
package testenv

import "os"

func FileHandlesRequired() bool {
	return os.Getenv("FU_REQUIRE_FILE_HANDLES") == "1"
}
