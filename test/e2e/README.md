# E2E Tests

End-to-end tests using [kubernetes-sigs/e2e-framework](https://github.com/kubernetes-sigs/e2e-framework).

## Structure

```
test/e2e/
├── go.mod                  # Separate test module
├── credit-authority/       # ADR-109: the Credit Authority's guarantees and isolation
├── infisical-eso/
├── platform-victoriametrics/
└── README.md
```

## Running Tests

### The Credit Authority (ADR-109)

Run through its gateway by `scripts/validate/cluster/109-credit-authority-isolation.sh`,
which port-forwards to it and sets `CREDITS_URL`. The suites skip unless
`CREDITS_URL`, `CREDITS_LEDGER` and `CREDITS_TOKEN` are set; a skipped suite proves
nothing.

### CI/CD

```bash
export KUBECONFIG=/path/to/kubeconfig
cd test/e2e
go test -v ./...
```

## Adding New Tests

1. Create test package under `test/e2e/`
2. Use e2e-framework patterns (TestMain, features, assessments)
3. Update this README

## Framework

Tests use standard `go test` with e2e-framework for:
- Kubeconfig resolution (`conf.ResolveKubeConfigFile()`)
- Feature-based organization
- Context-aware execution
- Setup/Teardown hooks
