package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/DanilaZanin/helm-unstick/internal/model"
)

// stored builds the Secret Helm would keep for one revision. payloadVersion and payloadStatus
// are what the release inside the record says; the labels say version and status.
func stored(t *testing.T, name string, version int, status string, rv string) *corev1.Secret {
	t.Helper()
	return storedAs(t, name, version, status, rv, name, version, status)
}

func storedAs(t *testing.T, name string, version int, status, rv, payloadName string, payloadVersion int, payloadStatus string) *corev1.Secret {
	t.Helper()
	enc, err := encodeRaw(map[string]any{
		"name": payloadName, "version": payloadVersion, "namespace": "prod",
		"info": map[string]any{"status": payloadStatus, "description": "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: recordName(name, version), Namespace: "prod", ResourceVersion: rv,
			Labels: map[string]string{"owner": "helm", "name": name, "version": fmt.Sprint(version), "status": status, "createdAt": "100"},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": enc},
	}
}

func TestHistoryIsBuiltFromOneSnapshot(t *testing.T) {
	ctx := context.Background()
	kc := fake.NewSimpleClientset(
		stored(t, "web", 2, "deployed", "10"),
		stored(t, "web", 3, "pending-upgrade", "11"),
	)
	var lists atomic.Int32
	kc.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil
	})
	h, err := readHistory(ctx, kc, "", "prod", "web")
	if err != nil {
		t.Fatal(err)
	}
	if n := lists.Load(); n != 1 {
		t.Errorf("History made %d list calls: status, resourceVersion and write time must come from one snapshot", n)
	}
	latest, _ := h.Latest()
	if latest.Number != 3 || latest.Status != model.StatusPendingUpgrade || latest.Version != "11" || !latest.ModifiedAt.Equal(time.Unix(100, 0)) {
		t.Errorf("latest = %+v", latest)
	}
}

func TestHistoryRejectsRecordsThatDisagreeWithThemselves(t *testing.T) {
	tests := []struct {
		name string
		recs []*corev1.Secret
		want string
	}{
		{"undecodable", []*corev1.Secret{func() *corev1.Secret {
			s := stored(t, "web", 1, "pending-install", "1")
			s.Data["release"] = []byte("not-a-release")
			return s
		}()}, "cannot be decoded"},
		{"label says v1, the stored release is v2", []*corev1.Secret{
			storedAs(t, "web", 1, "deployed", "1", "web", 2, "deployed"),
		}, "version label 1 but the stored release is revision 2"},
		{"the corrupted v2 hides behind a readable v1", []*corev1.Secret{
			stored(t, "web", 1, "deployed", "1"),
			storedAs(t, "web", 2, "pending-upgrade", "2", "web", 1, "deployed"),
		}, "sh.helm.release.v1.web.v2"},
		{"two records claim revision 2", []*corev1.Secret{
			stored(t, "web", 1, "deployed", "1"),
			stored(t, "web", 2, "deployed", "2"),
			func() *corev1.Secret {
				s := stored(t, "web", 2, "pending-upgrade", "3")
				s.Name = "sh.helm.release.v1.web.v2.copy"
				return s
			}(),
		}, "more than one record"},
		{"the release inside belongs to another name", []*corev1.Secret{
			storedAs(t, "web", 1, "deployed", "1", "api", 1, "deployed"),
		}, `stored release is named "api"`},
		{"label status disagrees with the stored release", []*corev1.Secret{
			storedAs(t, "web", 1, "deployed", "1", "web", 1, "pending-upgrade"),
		}, "status label"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, len(tt.recs))
			for i, r := range tt.recs {
				objs[i] = r
			}
			_, err := readHistory(context.Background(), fake.NewSimpleClientset(objs...), "", "prod", "web")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestHistoryConfigMapDriver(t *testing.T) {
	s := stored(t, "web", 1, "deployed", "5")
	cm := &corev1.ConfigMap{ObjectMeta: s.ObjectMeta, Data: map[string]string{"release": string(s.Data["release"])}}
	h, err := readHistory(context.Background(), fake.NewSimpleClientset(cm), "configmap", "prod", "web")
	if err != nil || len(h.Revisions) != 1 || h.Revisions[0].Version != "5" {
		t.Fatalf("history = %+v, err = %v", h, err)
	}
}

func TestHistoryOfAMissingReleaseIsNotFound(t *testing.T) {
	_, err := readHistory(context.Background(), fake.NewSimpleClientset(), "", "prod", "web")
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestGuardRecordRefusesAMovedRecord(t *testing.T) {
	ctx := context.Background()
	kc := fake.NewSimpleClientset(stored(t, "web", 3, "pending-upgrade", "42"))
	from := model.Revision{Number: 3, Status: model.StatusPendingUpgrade, Version: "42"}
	if err := guardRecord(ctx, kc, "", "prod", "web", from); err != nil {
		t.Fatalf("an untouched record must pass: %v", err)
	}
	tests := []struct {
		name string
		from model.Revision
	}{
		{"resourceVersion moved", model.Revision{Number: 3, Status: model.StatusPendingUpgrade, Version: "41"}},
		{"status moved", model.Revision{Number: 3, Status: model.StatusFailed, Version: "42"}},
		{"no version to guard with", model.Revision{Number: 3, Status: model.StatusPendingUpgrade}},
	}
	for _, tt := range tests {
		err := guardRecord(ctx, kc, "", "prod", "web", tt.from)
		if err == nil {
			t.Errorf("%s: the guard let a moved record through", tt.name)
		}
		if tt.name != "no version to guard with" && !errors.Is(err, model.ErrConflict) {
			t.Errorf("%s: err = %v, want a conflict", tt.name, err)
		}
	}
	// a record that is gone entirely is not the one that was inspected
	if err := guardRecord(ctx, fake.NewSimpleClientset(), "", "prod", "web", from); !errors.Is(err, model.ErrConflict) {
		t.Errorf("missing record: err = %v", err)
	}
}

func TestMarkRecordFailedReturnsTheNewVersion(t *testing.T) {
	kc := fake.NewSimpleClientset(secretRecord(t, "42", "pending-upgrade"))
	kc.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		o := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
		o.ResourceVersion = "43"
		return true, o, nil
	})
	got, err := markRecordFailed(context.Background(), kc, "", "prod", "web", pending("42"), "x", clock)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusFailed || got.Version != "43" || got.Number != 3 {
		t.Errorf("returned %+v: the follow-up rollback is guarded with this record", got)
	}
}

// syncBuffer is a buffer that the goroutine under test writes and the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunCtxLetsTheWriteFinishAfterTheFirstInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	c := &Client{stderr: &out}
	started, finish := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	errc := make(chan error, 1)
	go func() {
		errc <- c.runCtx(ctx, "rollback", func() error {
			close(started)
			<-finish
			finished.Store(true)
			return nil
		})
	}()
	<-started
	cancel() // the first Ctrl-C
	select {
	case err := <-errc:
		t.Fatalf("runCtx returned (%v) while the SDK call was still writing: an interrupt must not abandon it", err)
	case <-time.After(100 * time.Millisecond):
	}
	if !strings.Contains(out.String(), "finishing the current write") || !strings.Contains(out.String(), "Ctrl-C again") {
		t.Errorf("the first interrupt must say what happens: %q", out.String())
	}
	close(finish)
	if err := <-errc; err != nil || !finished.Load() {
		t.Errorf("err = %v, finished = %v", err, finished.Load())
	}
}
