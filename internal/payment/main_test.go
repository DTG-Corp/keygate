package payment

import (
	"os"
	"testing"

	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../db/migrations")) }
