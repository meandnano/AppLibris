package scanner

import (
	"testing"

	"library/internal/storage"
)

// The identifiers below are this package's own test helpers, re-exported
// for the external scanner_test package.
//
// That package exists because the assertion capValue is there for — that
// nothing this package stores is a value internal/service would then refuse
// to save — has to import internal/service, and internal/service reaches
// this package back through internal/importer. An in-package test file
// cannot close that loop; an external one can, and re-exporting is what
// keeps it sharing the same fixtures as the in-package tests rather than
// growing a second copy of them.

// TestMissingGrace is the grace period the in-package tests sweep with.
const TestMissingGrace = testMissingGrace

func CapValue(path string, field storage.MetadataField, value string) string {
	return capValue(path, field, value)
}

func WriteTestEPUBWithOPF(t *testing.T, path, opfXML string) {
	writeTestEPUBWithOPF(t, path, opfXML)
}
