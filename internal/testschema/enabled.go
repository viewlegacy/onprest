//go:build !schema_validator

package testschema

const Enabled = false

func validateJSON(t TestingT, schema, value any, want bool) {}
