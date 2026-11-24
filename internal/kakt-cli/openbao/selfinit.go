package openbao

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrSelfInitFailed is an OpenBao that started self-initialization and did not
// finish it (ADR-117 §6.2). OpenBao itself refuses to unseal such a store and
// exits; Day-0 stops with this error and FAILS CLOSED: it deletes nothing --
// not openbao-db, its PVCs, nor its archive. Recovering is an administrator
// decision under ADR-092.
var ErrSelfInitFailed = errors.New("OpenBao self-initialization failed")

// selfInitMarker is what OpenBao logs when it finds an interrupted self-init
// ("error unsealing core ... self-initialization failed") before it exits.
const selfInitMarker = "self-initialization failed"

// podLogs returns a container's log tail; previous selects the last terminated
// instance. A variable so tests can supply logs.
var podLogs = func(ctx context.Context, cs kubernetes.Interface, pod, container string, previous bool) (string, error) {
	tail := int64(200)
	rc, err := cs.CoreV1().Pods(Namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: previous, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	return string(b), err
}

// SelfInitFailed reports, from the server pods' own logs, whether OpenBao has
// refused to unseal an interrupted self-initialization. It needs no connection
// to OpenBao: a pod in that state crash-loops and serves nothing.
func SelfInitFailed(ctx context.Context, cs kubernetes.Interface) (string, bool, error) {
	pods, err := cs.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: serverSelector})
	if err != nil {
		return "", false, fmt.Errorf("list OpenBao pods: %w", err)
	}
	for _, p := range pods.Items {
		for _, st := range p.Status.ContainerStatuses {
			if st.RestartCount == 0 && st.State.Terminated == nil {
				continue // never exited: nothing to read
			}
			for _, previous := range []bool{true, false} {
				logs, err := podLogs(ctx, cs, p.Name, st.Name, previous)
				if err != nil {
					continue
				}
				if strings.Contains(logs, selfInitMarker) {
					return p.Name, true, nil
				}
			}
		}
	}
	return "", false, nil
}

// WaitForStore waits for OpenBao to be serving, initialized and unsealed and
// returns a connected client -- or ErrSelfInitFailed as soon as the pods show an
// interrupted self-initialization, rather than waiting out the timeout.
func WaitForStore(ctx context.Context, cs kubernetes.Interface, kubeconfig string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if pod, failed, err := SelfInitFailed(ctx, cs); err == nil && failed {
			return nil, fmt.Errorf("%w on %s/%s. OpenBao refuses to unseal a store whose "+
				"self-initialization was interrupted, and so does Day-0: nothing was deleted -- "+
				"openbao-db, its volumes and its archive are as they were. Deciding how to recover "+
				"it is an administrator decision under ADR-092", ErrSelfInitFailed, Namespace, pod)
		}
		c, err := Connect(ctx, cs, kubeconfig)
		if err == nil {
			s, serr := c.SealStatus(ctx)
			if serr == nil && s.Initialized && !s.Sealed {
				return c, nil
			}
			c.Close()
			err = fmt.Errorf("status %+v %v", s, serr)
		}
		last = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("OpenBao is not serving, initialized and unsealed after %s (%v)", timeout, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}
