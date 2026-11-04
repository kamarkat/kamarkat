package postgres

import "github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"

// Compile-time assertion: Storage must satisfy IStorage.
var _ interfaces.IStorage = (*Storage)(nil)
