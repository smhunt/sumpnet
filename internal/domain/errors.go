// Package domain holds the sentinel errors and vocabulary shared across
// services (prompt_plan.md §7). Wire-level errors live in internal/codec.
package domain

import "errors"

// Sentinel errors. Services map them to transport codes (gRPC NotFound, ...).
var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidArgument = errors.New("invalid argument")
)
