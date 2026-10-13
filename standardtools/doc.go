// Package standardtools implements optional Pi-style read/write/edit tools and
// Codex-style exec_command/write_stdin tools over a pluggable IO Backend.
// File algorithms, output limits, terminal sessions and target binding belong
// to this package. Runtime provisioning, authorization and workspace preparation
// belong to the host. Package standardtools/local supplies an optional local IO
// implementation. Core has no dependency on either package.
package standardtools
