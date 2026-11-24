package controller

import (
	"context"
	"errors"
	"fmt"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
)

// ErrRestoreUnfinished refuses generation while the box's store is an unfinished restore.
var ErrRestoreUnfinished = errors.New("refusing to generate a secret value during an unfinished restore")

// openBaoNamespace is where openbao-db lives.
const openBaoNamespace = "platform-data"

// NewRestoreGuard is secrets.Store.RestoreGuard (ADR-117 §7). It uses only the
// state ADR-092 recovery already has: the live openbao-db Cluster is a restore
// (spec.bootstrap.recovery) and its incarnation's recovery record is not yet
// complete. In that window a value missing from the store was LOST, not never
// created, so it must be restored rather than regenerated. Once the record is
// complete -- or the store was never restored -- generation is ordinary again.
func NewRestoreGuard(c client.Client, bao *openbao.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		uid, restoring, err := openBaoIncarnation(ctx, c)
		if err != nil || !restoring {
			return err
		}
		done, err := bao.RecoveryComplete(ctx, openBaoDatabase, uid)
		if err != nil {
			return fmt.Errorf("cannot tell whether restored openbao-db incarnation %s is recovered: %w", uid, err)
		}
		if done {
			return nil
		}
		return fmt.Errorf("%w: openbao-db incarnation %s was restored and its recovery is not complete, so a "+
			"missing value was lost rather than never created; restore it instead of generating a new one "+
			"(ADR-117 §7, `kakt openbao recover` lists what is missing)", ErrRestoreUnfinished, uid)
	}
}

// openBaoIncarnation reads the live openbao-db Cluster: its UID, and whether it is
// a restore. An absent Cluster (a box being built) is not a restore.
func openBaoIncarnation(ctx context.Context, c client.Client) (string, bool, error) {
	var db cnpgv1.Cluster
	if err := c.Get(ctx, types.NamespacedName{Namespace: openBaoNamespace, Name: openBaoDatabase}, &db); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s/%s: %w", openBaoNamespace, openBaoDatabase, err)
	}
	restoring := db.Spec.Bootstrap != nil && db.Spec.Bootstrap.Recovery != nil
	return string(db.UID), restoring, nil
}
