#!/bin/sh
# Runs once on first boot (empty data volume) as the POSTGRES_USER superuser.
# The image is Alpine-based: POSIX sh only, no bash.
# Creates the pg_partman extension in the sumpnet DB and a separate DB for ChirpStack.
set -eu

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" <<EOSQL
  CREATE SCHEMA IF NOT EXISTS partman;
  CREATE EXTENSION IF NOT EXISTS pg_partman SCHEMA partman;
  CREATE ROLE chirpstack LOGIN PASSWORD '${CHIRPSTACK_DB_PASSWORD}';
  CREATE DATABASE chirpstack OWNER chirpstack;
EOSQL

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d chirpstack <<EOSQL
  CREATE EXTENSION IF NOT EXISTS pg_trgm;
  CREATE EXTENSION IF NOT EXISTS hstore;
EOSQL
