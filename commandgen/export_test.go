package commandgen

// IsReservedCommandName exposes isReservedCommandName to the external
// commandgen_test package, which can import cli (cli imports commandgen, so
// an in-package test cannot).
var IsReservedCommandName = isReservedCommandName
