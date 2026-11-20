# PostgreSQL for the billing engine's cluster (ADR-113 §3): CloudNativePG's
# system PostgreSQL 18 image with pg_partman added, and nothing else.
#
# The engine's schema creates pg_partman on an empty database, and a cluster
# cannot load an extension its image does not ship. CloudNativePG's image-volume
# extensions need PostgreSQL 18 and Kubernetes image volumes, and the boxes run
# Kubernetes 1.31 (docs/plans/k8s-cnpg-upgrade-and-restore-drill.md); once they
# can, this image is replaced by an extension volume.
#
# PostgreSQL 18, the major image-volume extensions require, so that replacement
# needs no major upgrade of the database. The engine ships 15 and 17 itself
# (lago docker-compose.yml and docker/Dockerfile at v1.55.0); 18 is proven by
# its deployment.
#
# Both inputs are pinned, so a rebuild never upgrades either: the base by digest,
# the package by its exact PGDG version. A version PGDG no longer serves fails
# the build rather than installing another. The engine uses pg_partman's
# functions only (partman.create_parent in its migration) and never runs its
# maintenance, so the background worker is not loaded.
FROM ghcr.io/cloudnative-pg/postgresql:18-system-bookworm@sha256:5774670ec4a849c08d14f9db34320dff32a1a5f4ad6d41601ac492966ded3ad9

ARG PG_PARTMAN_VERSION=5.5.0-1.pgdg12+1

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends "postgresql-18-partman=${PG_PARTMAN_VERSION}" \
 && rm -rf /var/lib/apt/lists/*

# The base image's postgres user, which CloudNativePG runs the instance as.
USER 26
