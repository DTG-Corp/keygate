package store_test

import (
	"os"
	"testing"

	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// In the external test package: testdb imports store.
func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../db/migrations")) }
