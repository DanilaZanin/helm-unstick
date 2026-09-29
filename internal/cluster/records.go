package cluster

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/DanilaZanin/helm-unstick/internal/model"
)

// Helm keeps each release revision in one Kubernetes object named sh.helm.release.v1.NAME.vN
// (a Secret, or a ConfigMap with the configmap driver). The labels name, version and status
// are readable without decoding the release. Reading and guarding the objects directly gives
// three things the SDK does not: undecodable records are noticed instead of skipped, the write
// time and resourceVersion of every record are known, and a status change can be made
// conditional on the record still being the one that was inspected.

// writeTimeout bounds one storage write that an interrupt is not allowed to cancel.
const writeTimeout = 30 * time.Second

const pendingSelector = "owner=helm,status in (pending-install,pending-upgrade,pending-rollback)"

// record is the metadata of one stored release revision.
type record struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Version   string    // resourceVersion
	Written   time.Time // latest of creation, createdAt and modifiedAt
	Data      []byte    // the encoded release, from the same object the metadata came from
}

func (r record) release() string { return r.Labels["name"] }

func recordName(release string, revision int) string {
	return fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, revision)
}

// written returns the newest of the timestamps a record carries.
func written(created time.Time, labels map[string]string) time.Time {
	latest := created
	for _, label := range []string{"createdAt", "modifiedAt"} {
		secs, err := strconv.ParseInt(labels[label], 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(secs, 0); t.After(latest) {
			latest = t
		}
	}
	return latest
}

// configMaps reports whether the driver stores releases in ConfigMaps. Only the secret and
// configmap drivers keep Kubernetes objects; the command line rejects every other driver.
func configMaps(driver string) bool { return driver == "configmap" || driver == "configmaps" }

// listRecords returns the release records of a namespace ("" means all) that match selector.
func listRecords(ctx context.Context, kc kubernetes.Interface, driver, namespace, selector string) ([]record, error) {
	var out []record
	opts := metav1.ListOptions{LabelSelector: selector, Limit: 500}
	for {
		if configMaps(driver) {
			list, err := kc.CoreV1().ConfigMaps(namespace).List(ctx, opts)
			if err != nil {
				return nil, err
			}
			for _, o := range list.Items {
				out = append(out, record{o.Name, o.Namespace, o.Labels, o.ResourceVersion, written(o.CreationTimestamp.Time, o.Labels), []byte(o.Data["release"])})
			}
			opts.Continue = list.Continue
		} else {
			list, err := kc.CoreV1().Secrets(namespace).List(ctx, opts)
			if err != nil {
				return nil, err
			}
			for _, o := range list.Items {
				out = append(out, record{o.Name, o.Namespace, o.Labels, o.ResourceVersion, written(o.CreationTimestamp.Time, o.Labels), o.Data["release"]})
			}
			opts.Continue = list.Continue
		}
		if opts.Continue == "" {
			return out, nil
		}
	}
}

// pendingReleases lists the (namespace, release) pairs that have a pending record.
func pendingReleases(ctx context.Context, kc kubernetes.Interface, driver, namespace string) ([]releaseKey, error) {
	recs, err := listRecords(ctx, kc, driver, namespace, pendingSelector)
	if err != nil {
		return nil, err
	}
	seen := map[releaseKey]bool{}
	for _, r := range recs {
		seen[releaseKey{r.Namespace, r.release()}] = true
	}
	keys := make([]releaseKey, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].namespace != keys[j].namespace {
			return keys[i].namespace < keys[j].namespace
		}
		return keys[i].name < keys[j].name
	})
	return keys, nil
}

// decodeRelease decodes the release field of a record the way the Helm SDK does: base64,
// gzip when the magic bytes are there, then JSON.
func decodeRelease(data []byte) (*release.Release, error) {
	b, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, err
	}
	if len(b) > 3 && bytes.Equal(b[:3], []byte{0x1f, 0x8b, 0x08}) {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		if b, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	var rel release.Release
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// readHistory builds the history of one release from a single List call, so the status, the
// resourceVersion and the write time of every revision describe the same moment. A record
// that cannot be decoded, or that disagrees with its own labels, fails the whole history:
// the SDK would skip an undecodable record without a word, and a corrupted v2 hiding behind a
// readable v1 must not be mistaken for a healthy release.
func readHistory(ctx context.Context, kc kubernetes.Interface, driver, namespace, name string) (model.History, error) {
	recs, err := listRecords(ctx, kc, driver, namespace, "owner=helm,name="+name)
	if err != nil {
		return model.History{}, fmt.Errorf("listing the storage records of %q: %w", name, err)
	}
	if len(recs) == 0 {
		return model.History{}, fmt.Errorf("release %q not found in namespace %q: %w", name, namespace, model.ErrNotFound)
	}
	h, err := buildHistory(namespace, name, recs)
	if err != nil {
		return model.History{}, fmt.Errorf("release %q: %w", name, err)
	}
	return h, nil
}

func buildHistory(namespace, name string, recs []record) (model.History, error) {
	claims := map[string]int{} // label version -> number of records that claim it
	for _, r := range recs {
		claims[r.Labels["version"]]++
	}
	h := model.History{Namespace: namespace, Release: name}
	var bad []string
	for _, r := range recs {
		n, err := strconv.Atoi(r.Labels["version"])
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s (the version label %q is not a number)", r.Name, r.Labels["version"]))
			continue
		}
		if claims[r.Labels["version"]] > 1 {
			bad = append(bad, fmt.Sprintf("%s (more than one record claims revision %d)", r.Name, n))
			continue
		}
		rel, err := decodeRelease(r.Data)
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s (cannot be decoded: %v)", r.Name, err))
		case rel.Version != n:
			bad = append(bad, fmt.Sprintf("%s (version label %d but the stored release is revision %d)", r.Name, n, rel.Version))
		case rel.Name != name:
			bad = append(bad, fmt.Sprintf("%s (stored release is named %q)", r.Name, rel.Name))
		case rel.Info != nil && string(rel.Info.Status) != r.Labels["status"]:
			bad = append(bad, fmt.Sprintf("%s (status label %q but the stored release is %s)", r.Name, r.Labels["status"], rel.Info.Status))
		default:
			rev := toRevision(rel)
			rev.ModifiedAt, rev.Version = r.Written, r.Version
			h.Revisions = append(h.Revisions, rev)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return model.History{}, fmt.Errorf("%d of %d stored records cannot be trusted: %s; the history is incomplete, so nothing is decided on it",
			len(bad), len(recs), strings.Join(bad, "; "))
	}
	return h, nil
}

// decodeRaw turns the release field of a record into a generic JSON document, so that fields
// this program does not know about (a newer Helm writes some) survive a rewrite untouched.
func decodeRaw(data []byte) (map[string]any, error) {
	b, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, err
	}
	if len(b) > 3 && bytes.Equal(b[:3], []byte{0x1f, 0x8b, 0x08}) {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		if b, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func encodeRaw(doc map[string]any) ([]byte, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(buf.Bytes())), nil
}

// guardRecord fails with model.ErrConflict unless the record of revision from.Number still
// has the status and resourceVersion that were inspected. It is the check made right before
// a rollback or an uninstall: the SDK call that follows takes no precondition, so this is the
// closest the write can get to being conditional.
func guardRecord(ctx context.Context, kc kubernetes.Interface, driver, namespace, release string, from model.Revision) error {
	if from.Version == "" {
		return fmt.Errorf("revision %d has no record version to guard the write with: refusing to act", from.Number)
	}
	name := recordName(release, from.Number)
	var labels map[string]string
	var rv string
	var err error
	if configMaps(driver) {
		var cm *corev1.ConfigMap
		cm, err = kc.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			labels, rv = cm.Labels, cm.ResourceVersion
		}
	} else {
		var sec *corev1.Secret
		sec, err = kc.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			labels, rv = sec.Labels, sec.ResourceVersion
		}
	}
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s: the record is gone: %w", name, model.ErrConflict)
	case err != nil:
		return fmt.Errorf("re-reading record %s: %w", name, err)
	case rv != from.Version:
		return fmt.Errorf("%s: resourceVersion is %s, inspected %s: %w", name, rv, from.Version, model.ErrConflict)
	case labels["status"] != string(from.Status):
		return fmt.Errorf("%s: status is %s, inspected %s: %w", name, labels["status"], from.Status, model.ErrConflict)
	}
	return nil
}

// markRecordFailed sets one release record to failed, but only if it is still the pending
// record that was inspected: same status and same resourceVersion. The update carries that
// resourceVersion, so the API server itself rejects the write if anything touched the record
// after it was read. A record that is no longer pending is never rewritten.
func markRecordFailed(ctx context.Context, kc kubernetes.Interface, driver, namespace, release string, from model.Revision, reason string, now time.Time) (model.Revision, error) {
	if !from.Status.IsPending() {
		return model.Revision{}, fmt.Errorf("revision %d is %s, not pending: refusing to rewrite it", from.Number, from.Status)
	}
	if from.Version == "" {
		return model.Revision{}, fmt.Errorf("revision %d has no record version to guard the write with: refusing to rewrite it", from.Number)
	}
	name := recordName(release, from.Number)
	conflict := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format+": %w", append([]any{name}, append(args, model.ErrConflict)...)...)
	}
	patch := func(labels map[string]string, resourceVersion string, data []byte) ([]byte, error) {
		if resourceVersion != from.Version {
			return nil, conflict("resourceVersion is %s, inspected %s", resourceVersion, from.Version)
		}
		if labels["status"] != string(from.Status) {
			return nil, conflict("status is %s, inspected %s", labels["status"], from.Status)
		}
		doc, err := decodeRaw(data)
		if err != nil {
			return nil, fmt.Errorf("%s: decoding the record: %w", name, err)
		}
		info, _ := doc["info"].(map[string]any)
		if info == nil || info["status"] != string(from.Status) {
			return nil, conflict("the stored release is not %s", from.Status)
		}
		info["status"] = string(model.StatusFailed)
		info["description"] = reason
		labels["status"] = string(model.StatusFailed)
		labels["modifiedAt"] = strconv.FormatInt(now.Unix(), 10)
		return encodeRaw(doc)
	}
	// Once the update is on its way an interrupt must not cancel it half-way: the request is
	// a single write, so it either lands or is rejected.
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	var (
		newVersion string
		err        error
	)
	if configMaps(driver) {
		cm, gerr := kc.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return model.Revision{}, fmt.Errorf("reading record %s: %w", name, gerr)
		}
		enc, perr := patch(cm.Labels, cm.ResourceVersion, []byte(cm.Data["release"]))
		if perr != nil {
			return model.Revision{}, perr
		}
		cm.Data["release"] = string(enc)
		if cerr := ctx.Err(); cerr != nil { // an interrupt before the write starts stops it
			return model.Revision{}, cerr
		}
		var updated *corev1.ConfigMap
		updated, err = kc.CoreV1().ConfigMaps(namespace).Update(write, cm, metav1.UpdateOptions{})
		if err == nil {
			newVersion = updated.ResourceVersion
		}
	} else {
		sec, gerr := kc.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return model.Revision{}, fmt.Errorf("reading record %s: %w", name, gerr)
		}
		enc, perr := patch(sec.Labels, sec.ResourceVersion, sec.Data["release"])
		if perr != nil {
			return model.Revision{}, perr
		}
		sec.Data["release"] = enc
		if cerr := ctx.Err(); cerr != nil {
			return model.Revision{}, cerr
		}
		var updated *corev1.Secret
		updated, err = kc.CoreV1().Secrets(namespace).Update(write, sec, metav1.UpdateOptions{})
		if err == nil {
			newVersion = updated.ResourceVersion
		}
	}
	switch {
	case err == nil:
		out := from
		out.Status, out.Version = model.StatusFailed, newVersion
		return out, nil
	case apierrors.IsConflict(err):
		return model.Revision{}, conflict("the API server rejected the update: %v", err)
	}
	return model.Revision{}, fmt.Errorf("updating record %s: %w", name, err)
}
