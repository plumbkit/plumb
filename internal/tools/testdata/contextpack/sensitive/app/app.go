// Package app checks requests against the vault's token.
package app

import "example.com/vault/vault"

// Authorise reports whether got is the vault's current token.
func Authorise(got string) bool { return got == vault.Token() }
