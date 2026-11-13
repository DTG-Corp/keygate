package store

// This file is compiled only into the test binary: Go ignores _test.go
// files for a normal build, so the production binary exports no test
// surface. The hooks live here in package store (not store_test) because
// orderCode is unexported, while the order tests use the external
// store_test package and reach these exported wrappers.

// SetOrderCodeGeneratorForTest swaps the order-code generator so a test
// can force collisions.
func SetOrderCodeGeneratorForTest(fn func() string) { orderCode = fn }

// OrderCodeGeneratorForTest returns the current generator, so a test can
// restore the one it replaced.
func OrderCodeGeneratorForTest() func() string { return orderCode }
