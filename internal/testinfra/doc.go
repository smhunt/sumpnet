// Package testinfra starts the infrastructure integration tests need
// (Postgres with pg_partman, Mosquitto) in testcontainers. Every helper is
// behind the `integration` build tag; this file keeps the package importable
// in normal builds.
package testinfra
