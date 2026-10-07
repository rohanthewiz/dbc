package scripts

// What the external tests (embed_test.go, package scripts_test) need from
// inside. They are external because they run script.Check, and script
// imports (through sdb, db and config) this package: an in-package test
// importing script would be an import cycle.
var (
	Files        = files
	SamplePrefix = samplePrefix
	NoConn       = noConn
	NoConn2      = noConn2
)
