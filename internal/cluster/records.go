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

const pendingSelector = "owner=helm,status in (pending-install,pending-upgrade,pending-rollback)"

// record is the metadata of one stored release revision.
type record struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Version   string    // resourceVersion
	Written   time.Time // latest of creation, createdAt and modifiedAt
}

func (r record) release() string { return r.Labels["name"] }

func (r record) revision() (int, bool) {
	n, err := strconv.Atoi(r.Labels["version"])
	return n, err == nil
}

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
				out = append(out, record{o.Name, o.Namespace, o.Labels, o.ResourceVersion, written(o.CreationTimestamp.Time, o.Labels)})
			}
			opts.Continue = list.Continue
		} else {
			list, err := kc.CoreV1().Secrets(namespace).List(ctx, opts)
			if err != nil {
				return nil, err
			}
			for _, o := range list.Items {
				out = append(out, record{o.Name, o.Namespace, o.Labels, o.ResourceVersion, written(o.CreationTimestamp.Time, o.Labels)})
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

// checkDecoded compares the records of a release with the revisions the SDK managed to decode.
// The SDK skips a record it cannot decode without a word, which would hide a revision (maybe
// the pending one) from every decision made on the history.
func checkDecoded(recs []record, decoded map[int]bool) error {
	var bad []string
	for _, r := range recs {
		n, ok := r.revision()
		if !ok || !decoded[n] {
			bad = append(bad, r.Name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%d of %d stored records cannot be decoded (%s); the history is incomplete, so nothing is decided on it",
		len(bad), len(recs), strings.Join(bad, ", "))
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

// markRecordFailed sets one release record to failed, but only if it is still the pending
// record that was inspected: same status and same resourceVersion. The update carries that
// resourceVersion, so the API server itself rejects the write if anything touched the record
// after it was read. A record that is no longer pending is never rewritten.
func markRecordFailed(ctx context.Context, kc kubernetes.Interface, driver, namespace, release string, from model.Revision, reason string, now time.Time) error {
	if !from.Status.IsPending() {
		return fmt.Errorf("revision %d is %s, not pending: refusing to rewrite it", from.Number, from.Status)
	}
	if from.Version == "" {
		return fmt.Errorf("revision %d has no record version to guard the write with: refusing to rewrite it", from.Number)
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

	var err error
	if configMaps(driver) {
		cm, gerr := kc.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return fmt.Errorf("reading record %s: %w", name, gerr)
		}
		enc, perr := patch(cm.Labels, cm.ResourceVersion, []byte(cm.Data["release"]))
		if perr != nil {
			return perr
		}
		cm.Data["release"] = string(enc)
		_, err = kc.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	} else {
		sec, gerr := kc.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return fmt.Errorf("reading record %s: %w", name, gerr)
		}
		enc, perr := patch(sec.Labels, sec.ResourceVersion, sec.Data["release"])
		if perr != nil {
			return perr
		}
		sec.Data["release"] = enc
		_, err = kc.CoreV1().Secrets(namespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	switch {
	case err == nil:
		return nil
	case apierrors.IsConflict(err):
		return conflict("the API server rejected the update: %v", err)
	}
	return fmt.Errorf("updating record %s: %w", name, err)
}
