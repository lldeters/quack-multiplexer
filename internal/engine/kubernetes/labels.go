package kubernetes

import "k8s.io/apimachinery/pkg/labels"

// Labels and annotations on engine pods and token secrets. The label set
// identifies the pods of one gateway instance; the annotations carry what is
// needed to rebuild the registry after a gateway restart (engine discovery).
const (
	LabelManagedBy   = "app.kubernetes.io/managed-by"
	LabelName        = "app.kubernetes.io/name"
	LabelComponent   = "app.kubernetes.io/component"
	LabelAppInstance = "app.kubernetes.io/instance"

	LabelInstance   = "quackmux.io/instance"
	LabelKeyHash    = "quackmux.io/key-hash"
	LabelShareLevel = "quackmux.io/share-level"
	LabelProfile    = "quackmux.io/profile"
	LabelWarm       = "quackmux.io/warm"
	// LabelEngine marks a token secret with the name of its engine pod.
	LabelEngine = "quackmux.io/engine"

	AnnotationKey         = "quackmux.io/key"
	AnnotationUser        = "quackmux.io/user"
	AnnotationProfile     = "quackmux.io/profile"
	AnnotationSpecHash    = "quackmux.io/spec-hash"
	AnnotationTokenSecret = "quackmux.io/token-secret"
	// AnnotationInitSQLHash is the SHA-256 of the init SQL, which lives in
	// the engine's secret: a changed init SQL changes the spec hash.
	AnnotationInitSQLHash = "quackmux.io/init-sql-sha256"
	// AnnotationLastLaunch on a data volume claim records the last launch
	// of an engine with that volume (RFC 3339).
	AnnotationLastLaunch = "quackmux.io/last-launch"

	// annotationDefaultContainer makes kubectl logs/exec pick the engine
	// container when a pod template adds sidecars.
	annotationDefaultContainer = "kubectl.kubernetes.io/default-container"

	managedByValue      = "quackmux"
	nameValue           = "quack-engine"
	componentValue      = "engine"
	tokenComponentValue = "engine-token"
	// dataComponentValue marks the data volume claims of persistent profiles.
	dataComponentValue = "engine-data"
)

// Fixed names inside engine pods.
const (
	// ContainerName is the name of the engine container.
	ContainerName = "engine"
	// PortName names the Quack port of the engine container.
	PortName = "quack"

	tmpVolumeName    = "tmp"
	tmpVolumeAltName = "quackmux-tmp"
	tmpMountPath     = "/tmp"

	// dataVolumeName is the pod volume of the data volume claim (profiles
	// with persistence). Containers of a podTemplate may mount it by name.
	dataVolumeName = "quackmux-data"

	// tokenSecretSuffix is appended to the pod name for its token secret.
	tokenSecretSuffix = "-token"
	// tokenDataKey is the key of the token inside the secret.
	tokenDataKey = "token"
	// initSQLDataKey holds the rendered init SQL in the same secret: it may
	// carry credentials (CREATE SECRET), which must not show up in the pod
	// spec for everybody allowed to read pods.
	initSQLDataKey = "init.sql"

	// eventComponent is the source of the Kubernetes events we record and
	// the field manager of the objects we write.
	eventComponent = "quackmux"
)

// tokenSecretName returns the name of the token secret of a pod.
func tokenSecretName(podName string) string { return podName + tokenSecretSuffix }

// instanceSelector selects the pods and token secrets of one gateway
// instance: app.kubernetes.io/managed-by=quackmux,quackmux.io/instance=<instance>.
func instanceSelector(instance string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{LabelManagedBy: managedByValue, LabelInstance: instance})
}
