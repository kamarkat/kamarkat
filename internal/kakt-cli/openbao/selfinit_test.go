package openbao

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func serverPod(restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-0", Namespace: Namespace,
			Labels: map[string]string{"app.kubernetes.io/name": "openbao", "component": "server"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "openbao", RestartCount: restarts}}},
	}
}

func withLogs(t *testing.T, previous string) {
	t.Helper()
	orig := podLogs
	podLogs = func(_ context.Context, _ kubernetes.Interface, _, _ string, prev bool) (string, error) {
		if prev {
			return previous, nil
		}
		return "", nil
	}
	t.Cleanup(func() { podLogs = orig })
}

// An interrupted self-init is reported at once, by name, without waiting out the
// timeout -- and before any connection is attempted (none could succeed).
func TestWaitForStoreFailsClosedOnAnInterruptedSelfInit(t *testing.T) {
	withLogs(t, `[ERROR] core: error unsealing core: error="self-initialization failed"`)
	cs := fake.NewSimpleClientset(serverPod(3))
	start := time.Now()
	_, err := WaitForStore(context.Background(), cs, "", time.Hour)
	if !errors.Is(err, ErrSelfInitFailed) {
		t.Fatalf("got %v, want ErrSelfInitFailed", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the failure waited instead of being reported at once")
	}
	if pods, _ := cs.CoreV1().Pods(Namespace).List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 1 {
		t.Fatal("something was deleted")
	}
}

// A pod that merely restarted is not a failed self-init.
func TestAnOrdinaryRestartIsNotASelfInitFailure(t *testing.T) {
	withLogs(t, `[INFO] core: vault is unsealed`)
	if _, failed, err := SelfInitFailed(context.Background(), fake.NewSimpleClientset(serverPod(1))); err != nil || failed {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	withLogs(t, `self-initialization failed`)
	if _, failed, _ := SelfInitFailed(context.Background(), fake.NewSimpleClientset(serverPod(0))); failed {
		t.Fatal("a pod that never exited was read as failed")
	}
}
