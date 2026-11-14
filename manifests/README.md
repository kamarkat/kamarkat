# Manifests Directory

This directory contains infrastructure definitions for the Kamarkat Platform.

## Structure

- `core/` - Bootstrap components (CAPI operator, providers)
- `classes/` - ClusterClass definitions (ubuntu topologies)

## Usage

All manifests are embedded in the CLI binary via `go:embed` and applied during bootstrap.
