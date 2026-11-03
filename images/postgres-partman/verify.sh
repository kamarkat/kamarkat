#!/usr/bin/env bash
# Verifies a built postgres-partman image before its digest is pinned (ADR-113 §3):
# it is PostgreSQL 18, pg_partman is the pinned version, and the extension
# creates and serves the one function the billing engine calls, with the
# arguments its migration passes.
#
#   verify.sh <image>
#
# Runs a throwaway instance inside the image as its own user: initdb, start on
# a socket only, exercise, and exit. Nothing is published by this script.
set -euo pipefail

image="${1:?usage: verify.sh <image>}"
want_major=18
want_partman="$(sed -n 's/^ARG PG_PARTMAN_VERSION=\([0-9.]*\)-.*/\1/p' "$(dirname "$0")/Dockerfile")"
[ -n "$want_partman" ] || { echo "the Dockerfile pins no PG_PARTMAN_VERSION" >&2; exit 1; }

docker run --rm --entrypoint bash \
  -e WANT_MAJOR="$want_major" -e WANT_PARTMAN="$want_partman" \
  "$image" -c '
set -euo pipefail
bin=/usr/lib/postgresql/$WANT_MAJOR/bin
[ -x "$bin/postgres" ] || { echo "no PostgreSQL $WANT_MAJOR in the image" >&2; exit 1; }
data=$(mktemp -d)
"$bin/initdb" -D "$data" -U postgres >/dev/null
"$bin/pg_ctl" -D "$data" -o "-c listen_addresses= -k /tmp" -w start >/dev/null
sql() { "$bin/psql" -h /tmp -U postgres -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }

major=$(sql "SHOW server_version_num")
[ "${major:0:2}" = "$WANT_MAJOR" ] || { echo "server is $major, want $WANT_MAJOR" >&2; exit 1; }

sql "CREATE SCHEMA partman; CREATE EXTENSION pg_partman SCHEMA partman" >/dev/null
have=$(sql "SELECT extversion FROM pg_extension WHERE extname = '\''pg_partman'\''")
[ "$have" = "$WANT_PARTMAN" ] || { echo "pg_partman is $have, want $WANT_PARTMAN" >&2; exit 1; }

# The call the engine makes (db/migrate/20260109132143_partition_enriched_events.rb).
sql "CREATE TABLE public.events (t timestamptz NOT NULL) PARTITION BY RANGE (t)" >/dev/null
sql "SELECT partman.create_parent(p_parent_table := '\''public.events'\'', p_control := '\''t'\'', p_interval := '\''1 month'\'', p_type := '\''range'\'', p_premake := 3)" >/dev/null
parts=$(sql "SELECT count(*) FROM partman.show_partitions('\''public.events'\'')")
[ "$parts" -gt 0 ] || { echo "create_parent made no partitions" >&2; exit 1; }

"$bin/pg_ctl" -D "$data" -w stop >/dev/null
echo "PostgreSQL $major with pg_partman $have: create_parent made $parts partitions"
'
