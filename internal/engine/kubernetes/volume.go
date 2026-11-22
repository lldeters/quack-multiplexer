package kubernetes

// Data volumes of profiles with persistence (profiles.<name>.persistence):
// one PersistentVolumeClaim per engine key, created by Launch before the
// pod and never deleted by the gateway on its own. The claim name is a pure
// function of the gateway instance and the key, so a restarted gateway finds
// the claims again without any state; labels and annotations identify the
// key, and a claim that does not carry exactly this instance and key is never
// mounted. The claims have no owner reference: they outlive their engines
// (and the gateway's own objects). The manager makes sure that at most one
// engine per key exists (internal/engine/persistence.go); the access mode
// ReadWriteOncePod makes the scheduler refuse a second pod of a claim as
// well.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

const (
	// dataClaimPrefix starts the names of data volume claims. It is fixed
	// (provider.kubernetes.namePrefix does not apply): a changed prefix
	// must not lose the data of every key.
	dataClaimPrefix = "quackmux-data"
	// dataClaimHashLength is the number of hex digits of the key digest at
	// the end of a claim name (64 bits).
	dataClaimHashLength = 16
)

// errForeignClaim marks a claim that exists under the name of a key's data
// volume but was not created by this gateway instance for that key.
var errForeignClaim = errors.New("refusing to mount a claim that does not belong to the engine key")

// dataClaimName returns the name of the data volume claim of an engine key:
// quackmux-data-<level>-<owner>-<subdomain>-<profile>, shortened so that the
// name stays a DNS-1123 label, then a digest of the instance and the whole
// key. The digest keeps keys apart whose readable parts look alike ("Alice"
// and "alice", owners cut short), and the instance keeps two gateways in one
// namespace apart. The name depends on nothing else, so it never changes for
// a key: changing this function loses every volume.
func dataClaimName(instance string, key engine.Key) string {
	sum := sha256.Sum256([]byte("quackmux/data-claim\x00" + instance + "\x00" + key.String()))
	return buildName(hex.EncodeToString(sum[:])[:dataClaimHashLength],
		dataClaimPrefix, strings.ToLower(string(key.ShareLevel)), key.Owner, key.Subdomain, key.Profile)
}

// dataSelector selects the data volume claims of one gateway instance.
func dataSelector(instance string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{
		LabelManagedBy: managedByValue, LabelInstance: instance, LabelComponent: dataComponentValue,
	})
}

// dataClaim renders the data volume claim of a key. user is the user whose
// session causes the creation.
func (r *renderer) dataClaim(name string, key engine.Key, profileName, user string, ps *config.PersistenceConfig, now time.Time) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
			// The gateway's labels and annotations win over the profile's.
			Labels: mergeMaps(ps.Labels, map[string]string{
				LabelManagedBy:   managedByValue,
				LabelName:        nameValue,
				LabelComponent:   dataComponentValue,
				LabelAppInstance: r.instance,
				LabelInstance:    r.instance,
				LabelKeyHash:     key.Hash(),
				LabelShareLevel:  strings.ToLower(string(key.ShareLevel)),
				LabelProfile:     labelValue(profileName),
			}),
			Annotations: mergeMaps(ps.Annotations, map[string]string{
				AnnotationKey:        key.String(),
				AnnotationUser:       user,
				AnnotationProfile:    profileName,
				AnnotationLastLaunch: now.UTC().Format(time.RFC3339),
			}),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{ps.AccessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: ps.Size.DeepCopy()},
			},
			VolumeMode: ptrTo(corev1.PersistentVolumeFilesystem),
		},
	}
	if ps.StorageClassName != "" {
		pvc.Spec.StorageClassName = ptrTo(ps.StorageClassName)
	}
	return pvc
}

// addDataVolume mounts the data volume claim at the profile's mount path.
func addDataVolume(spec *corev1.PodSpec, c *corev1.Container, claim string, ps *config.PersistenceConfig) {
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: dataVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		},
	})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: dataVolumeName, MountPath: ps.MountPath})
}

// validatePersistence checks the parts of a rendered placeholder pod and
// claim of a profile with persistence that the API server or the scheduler
// would not catch.
func (r *renderer) validatePersistence(name string, prof config.Profile, pod *corev1.Pod) error {
	ps := prof.Persistence
	base := field.NewPath("profiles").Key(name)
	var errs []error
	volumes := 0
	for _, v := range pod.Spec.Volumes {
		if v.Name == dataVolumeName {
			volumes++
		}
	}
	if volumes > 1 {
		errs = append(errs, fmt.Errorf("%s: the podTemplate must not define a volume named %q (the data volume)",
			base.Child("podTemplate"), dataVolumeName))
	}
	// addDataVolume appended the last mount; any other one at the mount
	// path comes from the template and would be a duplicate mount path.
	c := &pod.Spec.Containers[engineContainer(&pod.Spec)]
	for _, m := range c.VolumeMounts[:len(c.VolumeMounts)-1] {
		if path.Clean(m.MountPath) == ps.MountPath {
			errs = append(errs, fmt.Errorf("%s: the podTemplate's engine container already mounts %s at %s",
				base.Child("persistence", "mountPath"), m.Name, ps.MountPath))
		}
	}
	if pod.Spec.NodeName != "" {
		// A pod with spec.nodeName skips the scheduler, and with it the
		// ReadWriteOncePod check (verified in kind with a hostPath volume).
		errs = append(errs, fmt.Errorf("%s: spec.nodeName cannot be used with persistence (it bypasses the scheduler's ReadWriteOncePod check); use a nodeSelector",
			base.Child("podTemplate")))
	}
	if sc := ps.StorageClassName; sc != "" {
		if el := validation.IsDNS1123Subdomain(sc); len(el) > 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a valid storage class name: %s",
				base.Child("persistence", "storageClassName"), sc, strings.Join(el, "; ")))
		}
	}
	claim := r.dataClaim(dataClaimName(r.instance, placeholderKey), placeholderKey, name, placeholderUser, ps, time.Unix(0, 0))
	if el := metav1validation.ValidateLabels(claim.Labels, base.Child("persistence", "labels")); len(el) > 0 {
		errs = append(errs, el.ToAggregate())
	}
	if el := apivalidation.ValidateAnnotations(claim.Annotations, base.Child("persistence", "annotations")); len(el) > 0 {
		errs = append(errs, el.ToAggregate())
	}
	return errors.Join(errs...)
}

// ensureDataClaim makes sure that the data volume claim of the key exists and
// belongs to it: it creates a missing claim and refuses claims that are being
// deleted, lost their volume, or were made by somebody else under that name
// (another key with a colliding digest, another gateway instance, a user).
// For an existing claim it records the launch (best effort).
func (p *Provider) ensureDataClaim(ctx context.Context, req engine.LaunchRequest) (string, error) {
	ps := req.Profile.Persistence
	name := dataClaimName(p.instance, req.Key)
	pvcs := p.client.CoreV1().PersistentVolumeClaims(p.namespace)
	now := p.now()
	got, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		want := p.dataClaim(name, req.Key, req.ProfileName, req.User, ps, now)
		if _, err = pvcs.Create(ctx, want, metav1.CreateOptions{FieldManager: eventComponent}); err == nil {
			p.log.Info("created data volume claim", "claim", name, "key", req.Key.String(), "size", ps.Size.String(),
				"accessMode", ps.AccessMode, "storageClass", ps.StorageClassName)
			return name, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create data volume claim %s: %w", name, err)
		}
		got, err = pvcs.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("read data volume claim %s: %w", name, err)
	}
	if err := p.checkDataClaim(got, req.Key); err != nil {
		return "", err
	}
	if m := got.Spec.AccessModes; len(m) != 1 || m[0] != ps.AccessMode {
		p.warnOnce(name, "the data volume claim keeps the access modes it was created with",
			"claim", name, "accessModes", m, "profile", ps.AccessMode)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		AnnotationLastLaunch: now.UTC().Format(time.RFC3339),
	}}})
	if _, err := pvcs.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: eventComponent}); err != nil {
		p.log.Debug("recording the launch on the data volume claim failed", "claim", name, "err", err)
	}
	return name, nil
}

// checkSoleEngine makes sure that no other pod of the key exists before the
// pod of a persistent key is created; it asks the API server, not the cache.
// The manager launches an engine for such a key only when its registry holds
// no other engine of the key. This also covers the pods the registry does not
// know: one that the initial List skipped because its token could not be
// read, or one that another gateway process created. Such a pod is replayed
// to the manager, which adopts it once its token can be read.
func (p *Provider) checkSoleEngine(ctx context.Context, req engine.LaunchRequest) error {
	sel := labels.SelectorFromSet(labels.Set{
		LabelManagedBy: managedByValue, LabelInstance: p.instance, LabelKeyHash: req.Key.Hash(),
	})
	list, err := p.client.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return fmt.Errorf("list the engine pods of key %s: %w", req.Key.String(), err)
	}
	for i := range list.Items {
		if pod := &list.Items[i]; pod.Name != req.ID {
			p.requestReplay(nil)
			return fmt.Errorf("%w (engine pod %s exists; only one engine may mount data volume claim %s)",
				engine.ErrHandover, pod.Name, dataClaimName(p.instance, req.Key))
		}
	}
	return nil
}

// checkDataClaim verifies that an existing claim is the data volume of key
// and usable.
func (p *Provider) checkDataClaim(pvc *corev1.PersistentVolumeClaim, key engine.Key) error {
	if !p.ownsClaim(pvc) || pvc.Annotations[AnnotationKey] != key.String() {
		return fmt.Errorf("%w: claim %s exists but was not created by this gateway instance for engine key %s",
			errForeignClaim, pvc.Name, key.String())
	}
	if pvc.DeletionTimestamp != nil {
		return fmt.Errorf("data volume claim %s is being deleted; try again once it is gone", pvc.Name)
	}
	if pvc.Status.Phase == corev1.ClaimLost {
		return fmt.Errorf("data volume claim %s has lost its volume (phase Lost)", pvc.Name)
	}
	return nil
}

// ownsClaim reports whether a claim is a data volume claim of this instance.
func (p *Provider) ownsClaim(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Namespace == p.namespace && dataSelector(p.instance).Matches(labels.Set(pvc.Labels))
}

// Volumes implements engine.VolumeProvider: the data volume claims of this
// gateway instance. Claims whose key annotation does not parse are skipped.
func (p *Provider) Volumes(ctx context.Context) ([]engine.Volume, error) {
	list, err := p.client.CoreV1().PersistentVolumeClaims(p.namespace).List(ctx,
		metav1.ListOptions{LabelSelector: dataSelector(p.instance).String()})
	if err != nil {
		return nil, fmt.Errorf("kubernetes provider: list data volume claims: %w", err)
	}
	out := make([]engine.Volume, 0, len(list.Items))
	for i := range list.Items {
		pvc := &list.Items[i]
		key, err := engine.ParseKey(pvc.Annotations[AnnotationKey])
		if err != nil {
			p.warnOnce(pvc.Name, "ignoring a data volume claim with an invalid key annotation", "claim", pvc.Name, "err", err)
			continue
		}
		v := engine.Volume{
			Name: pvc.Name, Key: key.String(), ShareLevel: string(key.ShareLevel), Owner: key.Owner,
			Subdomain: key.Subdomain, Profile: key.Profile, User: pvc.Annotations[AnnotationUser],
			Phase: string(pvc.Status.Phase), CreatedAt: pvc.CreationTimestamp.Time,
			Deleting: pvc.DeletionTimestamp != nil, EngineKey: key,
		}
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			v.Size = q.String()
		} else if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			v.Size = q.String()
		}
		if sc := pvc.Spec.StorageClassName; sc != nil {
			v.StorageClass = *sc
		}
		for _, m := range pvc.Spec.AccessModes {
			v.AccessModes = append(v.AccessModes, string(m))
		}
		if t, err := time.Parse(time.RFC3339, pvc.Annotations[AnnotationLastLaunch]); err == nil {
			v.LastLaunch = t
		}
		out = append(out, v)
	}
	return out, nil
}

// DeleteVolume implements engine.VolumeProvider. Only claims of this
// instance are deleted, and only the object that was read (UID
// precondition). Kubernetes keeps a claim that a pod still uses until that
// pod is gone (finalizer kubernetes.io/pvc-protection).
func (p *Provider) DeleteVolume(ctx context.Context, name string) error {
	pvcs := p.client.CoreV1().PersistentVolumeClaims(p.namespace)
	pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return engine.ErrVolumeNotFound
	}
	if err != nil {
		return fmt.Errorf("read data volume claim %s: %w", name, err)
	}
	if !p.ownsClaim(pvc) {
		return engine.ErrVolumeNotFound
	}
	uid := pvc.UID
	err = pvcs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) {
		return engine.ErrVolumeNotFound
	}
	if err != nil {
		return fmt.Errorf("delete data volume claim %s: %w", name, err)
	}
	p.log.Info("deleting data volume claim", "claim", name, "key", pvc.Annotations[AnnotationKey])
	return nil
}
