package podreplace

import (
	"context"
	"encoding/json"
	"fmt"
	yaml2 "github.com/ghodss/yaml"
	"github.com/loft-sh/devspace/pkg/devspace/config"
	"github.com/loft-sh/devspace/pkg/devspace/config/loader"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	dependencytypes "github.com/loft-sh/devspace/pkg/devspace/dependency/types"
	"github.com/loft-sh/devspace/pkg/devspace/deploy/deployer/util"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/devspace/services/targetselector"
	"github.com/loft-sh/devspace/pkg/util/encoding"
	"github.com/loft-sh/devspace/pkg/util/hash"
	"github.com/loft-sh/devspace/pkg/util/imageselector"
	"github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/ptr"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"strconv"
	"time"
)

const (
	ParentKindAnnotation        = "devspace.sh/parent-kind"
	ParentNameAnnotation        = "devspace.sh/parent-name"
	ParentHashAnnotation        = "devspace.sh/parent-hash"
	ReplaceConfigHashAnnotation = "devspace.sh/config-hash"

	ReplicasAnnotation = "devspace.sh/replicas"
)

type PodReplacer interface {
	// ReplacePod will try to replace a pod with the given config
	ReplacePod(ctx context.Context, client kubectl.Client, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod, log log.Logger) error

	// RevertReplacePod will try to revert a pod replacement with the given config
	RevertReplacePod(ctx context.Context, client kubectl.Client, replacePod *latest.ReplacePod, log log.Logger) (*kubectl.SelectedPodContainer, error)
}

func NewPodReplacer() PodReplacer {
	return &replacer{}
}

type replacer struct{}

func (p *replacer) RevertReplacePod(ctx context.Context, client kubectl.Client, replacePod *latest.ReplacePod, log log.Logger) (*kubectl.SelectedPodContainer, error) {
	// check if there is a replaced pod in the target namespace
	log.StartWait("Try to find replaced pod...")
	defer log.StopWait()

	// try to find a single patched pod
	selectedPod, err := findSingleReplacedPod(ctx, client, replacePod, 4, log)
	if err != nil {
		return nil, errors.Wrap(err, "find patched pod")
	} else if selectedPod == nil {
		return nil, nil
	}

	if selectedPod.Pod.Annotations == nil || selectedPod.Pod.Annotations[ParentKindAnnotation] == "" || selectedPod.Pod.Annotations[ParentNameAnnotation] == "" {
		return selectedPod, deleteAndWait(ctx, client, selectedPod.Pod, log)
	}

	parent, err := getParentFromReplaced(ctx, client, selectedPod.Pod)
	if err != nil {
		log.Infof("Error getting Parent of replaced Pod %s/%s: %v", selectedPod.Pod.Namespace, selectedPod.Pod.Name, err)
		return selectedPod, deleteAndWait(ctx, client, selectedPod.Pod, log)
	}

	// delete replaced pod
	err = deleteAndWait(ctx, client, selectedPod.Pod, log)
	if err != nil {
		return nil, errors.Wrap(err, "delete replaced pod")
	}

	// scale up parent
	log.StartWait("Scaling up parent of replaced pod...")
	err = scaleUpParent(ctx, client, parent)
	if err != nil {
		return nil, err
	}

	return selectedPod, nil
}

func (p *replacer) ReplacePod(ctx context.Context, client kubectl.Client, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod, log log.Logger) error {
	// check if there is a replaced pod in the target namespace
	log.StartWait("Try to find replaced pod...")
	defer log.StopWait()

	// try to find a single patched pod
	selectedPod, err := findSingleReplacedPod(ctx, client, replacePod, 2, log)
	if err != nil {
		return errors.Wrap(err, "find patched pod")
	} else if selectedPod != nil {
		shouldUpdate, err := updateNeeded(ctx, client, selectedPod, config, dependencies, replacePod, log)
		if err != nil {
			return err
		} else if shouldUpdate == false {
			return nil
		}
	}

	// try to find a single patchable object
	log.StartWait("Try to find replaceable pod...")
	container, parent, err := findSingleReplaceablePodParent(ctx, client, config, dependencies, replacePod, log)
	if err != nil {
		return err
	}

	// replace the pod
	log.StartWait(fmt.Sprintf("Replacing Pod %s/%s...", container.Pod.Namespace, container.Pod.Name))
	err = replace(ctx, client, container, parent, config, dependencies, replacePod, log)
	if err != nil {
		return err
	}

	log.Donef("Successfully replaced pod %s/%s", container.Pod.Namespace, container.Pod.Name)
	return nil
}

func updateNeeded(ctx context.Context, client kubectl.Client, pod *kubectl.SelectedPodContainer, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod, log log.Logger) (bool, error) {
	if pod.Pod.Annotations == nil || pod.Pod.Annotations[ParentKindAnnotation] == "" || pod.Pod.Annotations[ParentNameAnnotation] == "" {
		return true, deleteAndWait(ctx, client, pod.Pod, log)
	}

	parent, err := getParentFromReplaced(ctx, client, pod.Pod)
	if err != nil {
		log.Infof("Error getting Parent of replaced Pod %s/%s: %v", pod.Pod.Namespace, pod.Pod.Name, err)
		return true, deleteAndWait(ctx, client, pod.Pod, log)
	}

	parentHash, err := hashParentPodSpec(parent, config, dependencies, replacePod)
	if err != nil {
		return false, errors.Wrap(err, "hash parent")
	}

	configHash, err := hashConfig(replacePod)
	if err != nil {
		return false, errors.Wrap(err, "hash config")
	}

	// don't update if pod spec & config hash are the same
	if parentHash == pod.Pod.Annotations[ParentHashAnnotation] && configHash == pod.Pod.Annotations[ReplaceConfigHashAnnotation] {
		// make sure parent is downscaled
		err = scaleDownParent(ctx, client, parent)
		if err != nil {
			log.Warnf("Error scaling down parent: %v", err)
		}

		return false, nil
	}

	// delete replaced pod
	log.Info("Change detected for replaced Pod " + pod.Pod.Namespace + "/" + pod.Pod.Name)
	err = deleteAndWait(ctx, client, pod.Pod, log)
	if err != nil {
		return false, errors.Wrap(err, "delete replaced pod")
	}

	// scale up parent
	log.StartWait("Scaling up parent of replaced pod...")
	err = scaleUpParent(ctx, client, parent)
	if err != nil {
		return false, err
	}

	return true, nil
}

func getParentFromReplaced(ctx context.Context, client kubectl.Client, pod *corev1.Pod) (runtime.Object, error) {
	var (
		parent runtime.Object
		err    error
	)
	switch pod.Annotations[ParentKindAnnotation] {
	case "ReplicaSet":
		parent, err = client.KubeClient().AppsV1().ReplicaSets(pod.Namespace).Get(ctx, pod.Annotations[ParentNameAnnotation], metav1.GetOptions{})
	case "Deployment":
		parent, err = client.KubeClient().AppsV1().Deployments(pod.Namespace).Get(ctx, pod.Annotations[ParentNameAnnotation], metav1.GetOptions{})
	case "StatefulSet":
		parent, err = client.KubeClient().AppsV1().StatefulSets(pod.Namespace).Get(ctx, pod.Annotations[ParentNameAnnotation], metav1.GetOptions{})
	default:
		return nil, fmt.Errorf("unrecognized parent kind")
	}
	return parent, err
}

func scaleUpParent(ctx context.Context, client kubectl.Client, parent runtime.Object) error {
	clonedParent := parent.DeepCopyObject()
	metaParent, err := meta.Accessor(parent)
	if err != nil {
		return errors.Wrap(err, "parent accessor")
	}

	// check if required annotation is there
	annotations := metaParent.GetAnnotations()
	if annotations == nil || annotations[ReplicasAnnotation] == "" {
		return nil
	}

	// scale up parent
	oldReplica, err := strconv.Atoi(annotations[ReplicasAnnotation])
	if err != nil {
		return errors.Wrap(err, "parse old replicas")
	} else if oldReplica == 0 {
		return nil
	}

	oldReplica32 := int32(oldReplica)
	switch t := parent.(type) {
	case *appsv1.ReplicaSet:
		t.Spec.Replicas = &oldReplica32
	case *appsv1.Deployment:
		t.Spec.Replicas = &oldReplica32
	case *appsv1.StatefulSet:
		t.Spec.Replicas = &oldReplica32
	}

	// delete replicas annotation
	delete(annotations, ReplicasAnnotation)
	metaParent.SetAnnotations(annotations)

	// create patch
	patch := MergeFrom(clonedParent)
	bytes, err := patch.Data(parent)
	if err != nil {
		return errors.Wrap(err, "create parent patch")
	}

	// patch parent
	switch t := parent.(type) {
	case *appsv1.ReplicaSet:
		_, err = client.KubeClient().AppsV1().ReplicaSets(t.Namespace).Patch(ctx, t.Name, patch.Type(), bytes, metav1.PatchOptions{})
	case *appsv1.Deployment:
		_, err = client.KubeClient().AppsV1().Deployments(t.Namespace).Patch(ctx, t.Name, patch.Type(), bytes, metav1.PatchOptions{})
	case *appsv1.StatefulSet:
		_, err = client.KubeClient().AppsV1().StatefulSets(t.Namespace).Patch(ctx, t.Name, patch.Type(), bytes, metav1.PatchOptions{})
	}
	if err != nil {
		return errors.Wrap(err, "patch parent")
	}

	return nil
}

func deleteAndWait(ctx context.Context, client kubectl.Client, pod *corev1.Pod, log log.Logger) error {
	log.StartWait(fmt.Sprintf("Waiting for replaced pod " + pod.Namespace + "/" + pod.Name + " to get terminated..."))
	err := client.KubeClient().CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	err = wait.Poll(time.Second, time.Minute*2, func() (bool, error) {
		_, err = client.KubeClient().CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			if kerrors.IsNotFound(err) {
				return true, nil
			}

			return false, err
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	log.StopWait()
	log.Donef("Deleted replaced pod %s/%s", pod.Namespace, pod.Name)
	return nil
}

func replace(ctx context.Context, client kubectl.Client, pod *kubectl.SelectedPodContainer, parent runtime.Object, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod, log log.Logger) error {
	parentHash, err := hashParentPodSpec(parent, config, dependencies, replacePod)
	if err != nil {
		return errors.Wrap(err, "hash parent pod spec")
	}

	configHash, err := hashConfig(replacePod)
	if err != nil {
		return errors.Wrap(err, "hash config")
	}

	copiedPod := pod.Pod.DeepCopyObject().(*corev1.Pod)

	// replace the image name
	if replacePod.ReplaceImage != "" {
		err := replaceImageInPodSpec(&copiedPod.Spec, config, dependencies, replacePod)
		if err != nil {
			return err
		}
	}

	// apply the patches
	copiedPod, err = applyPodPatches(copiedPod, replacePod)
	if err != nil {
		return errors.Wrap(err, "apply pod patches")
	}

	// reset the metadata
	copiedPod.ObjectMeta = metav1.ObjectMeta{
		Name:        encoding.SafeConcatName(copiedPod.Name, "devspace"),
		Namespace:   copiedPod.Namespace,
		Labels:      copiedPod.Labels,
		Annotations: copiedPod.Annotations,
	}
	if copiedPod.Annotations == nil {
		copiedPod.Annotations = map[string]string{}
	}
	if copiedPod.Labels == nil {
		copiedPod.Labels = map[string]string{}
	}

	// make sure the pod-template-hash label is deleted
	delete(copiedPod.Labels, "pod-template-hash")
	delete(copiedPod.Labels, "controller-revision-hash")
	delete(copiedPod.Labels, "statefulset.kubernetes.io/pod-name")

	copiedPod.Labels[kubectl.ReplacedLabel] = "true"
	if replacePod.ImageName != "" {
		copiedPod.Labels[kubectl.ImageNameLabel] = replacePod.ImageName
	}
	copiedPod.Annotations[kubectl.MatchedContainerAnnotation] = pod.Container.Name
	copiedPod.Annotations[ParentHashAnnotation] = parentHash
	copiedPod.Annotations[ReplaceConfigHashAnnotation] = configHash

	// get pod spec from object
	switch t := parent.(type) {
	case *appsv1.ReplicaSet:
		copiedPod.Annotations[ParentNameAnnotation] = t.Name
		copiedPod.Annotations[ParentKindAnnotation] = "ReplicaSet"
	case *appsv1.Deployment:
		copiedPod.Annotations[ParentNameAnnotation] = t.Name
		copiedPod.Annotations[ParentKindAnnotation] = "Deployment"
	case *appsv1.StatefulSet:
		copiedPod.Annotations[ParentNameAnnotation] = t.Name
		copiedPod.Annotations[ParentKindAnnotation] = "StatefulSet"
	default:
		return fmt.Errorf("unrecognized object")
	}

	// scale down parent
	err = scaleDownParent(ctx, client, parent)
	if err != nil {
		return errors.Wrap(err, "scale down parent")
	}
	log.Donef("Scaled down %s %s/%s", copiedPod.Annotations[ParentKindAnnotation], copiedPod.Namespace, copiedPod.Annotations[ParentNameAnnotation])

	// wait until pod is in terminating mode
	log.StartWait("Waiting for Pod " + pod.Pod.Name + " to get terminated...")
	err = wait.Poll(time.Second*2, time.Minute*2, func() (bool, error) {
		pod, err := client.KubeClient().CoreV1().Pods(pod.Pod.Namespace).Get(ctx, pod.Pod.Name, metav1.GetOptions{})
		if err != nil {
			if kerrors.IsNotFound(err) {
				return true, nil
			}

			return false, err
		}

		// for non stateful set its enough if the pod is still terminating
		if pod.DeletionTimestamp != nil && copiedPod.Annotations[ParentKindAnnotation] != "StatefulSet" {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return errors.Wrap(err, "wait for original pod to terminate")
	}

	// create the new pod
	_, err = client.KubeClient().CoreV1().Pods(copiedPod.Namespace).Create(ctx, copiedPod, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "create copied pod")
	}

	return nil
}

func applyPodPatches(pod *corev1.Pod, replacePod *latest.ReplacePod) (*corev1.Pod, error) {
	if len(replacePod.Patches) == 0 {
		return pod.DeepCopy(), nil
	}

	raw, err := loader.ApplyPatchesOnObject(convertToInterface(pod), replacePod.Patches)
	if err != nil {
		return nil, err
	}

	// convert back
	rawJson := convertFromInterface(raw)
	retPod := &corev1.Pod{}
	err = json.Unmarshal(rawJson, retPod)
	if err != nil {
		return nil, err
	}

	return retPod, nil
}

func hashConfig(replacePod *latest.ReplacePod) (string, error) {
	out, err := yaml.Marshal(replacePod)
	if err != nil {
		return "", err
	}

	return hash.String(string(out)), nil
}

func hashParentPodSpec(obj runtime.Object, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod) (string, error) {
	cloned := obj.DeepCopyObject()
	var podSpec *corev1.PodTemplateSpec

	// get pod spec from object
	switch t := cloned.(type) {
	case *appsv1.ReplicaSet:
		podSpec = &t.Spec.Template
	case *appsv1.Deployment:
		podSpec = &t.Spec.Template
	case *appsv1.StatefulSet:
		podSpec = &t.Spec.Template
	default:
		return "", fmt.Errorf("unrecognized object")
	}

	// replace the image name
	if replacePod.ReplaceImage != "" {
		err := replaceImageInPodSpec(&podSpec.Spec, config, dependencies, replacePod)
		if err != nil {
			return "", err
		}
	}

	out, err := json.Marshal(podSpec)
	if err != nil {
		return "", err
	}

	return hash.String(string(out)), nil
}

func replaceImageInPodSpec(podSpec *corev1.PodSpec, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod) error {
	_, image, err := util.Replace(replacePod.ReplaceImage, config, dependencies, map[string]string{})
	if err != nil {
		return err
	}
	imageStr := fmt.Sprintf("%v", image)

	// either replace by labelSelector & containerName
	// or by resolved image name
	if replacePod.LabelSelector != nil {
		if len(podSpec.Containers) > 1 && replacePod.ContainerName == "" {
			return fmt.Errorf("pod spec has more than 1 containers and containerName is an empty string")
		} else if len(podSpec.Containers) == 0 {
			return fmt.Errorf("no containers in pod spec")
		}

		// exchange image name
		for i := range podSpec.Containers {
			if len(podSpec.Containers) == 1 {
				podSpec.Containers[i].Image = imageStr
				break
			} else if podSpec.Containers[i].Name == replacePod.ContainerName {
				podSpec.Containers[i].Image = imageStr
				break
			}
		}
	} else {
		imageSelector, err := imageselector.Resolve(replacePod.ImageName, config, dependencies)
		if err != nil {
			return err
		} else if len(imageSelector) != 1 {
			return fmt.Errorf("unexpected amount of image selectors resolved: %#+v", imageSelector)
		}

		// exchange image name
		for i := range podSpec.Containers {
			if len(podSpec.Containers) == 1 {
				podSpec.Containers[i].Image = imageStr
				break
			} else if imageselector.CompareImageNames(imageSelector[0], podSpec.Containers[i].Image) {
				podSpec.Containers[i].Image = imageStr
				break
			}
		}
	}

	return nil
}

func scaleDownParent(ctx context.Context, client kubectl.Client, obj runtime.Object) error {
	cloned := obj.DeepCopyObject()

	// update object based on type
	switch t := obj.(type) {
	case *appsv1.ReplicaSet:
		if t.Annotations == nil {
			t.Annotations = map[string]string{}
		}

		replicas := 1
		if t.Spec.Replicas != nil {
			replicas = int(*t.Spec.Replicas)
		}

		if replicas == 0 {
			return nil
		}

		t.Annotations[ReplicasAnnotation] = strconv.Itoa(replicas)
		t.Spec.Replicas = ptr.Int32(0)
		patch := MergeFrom(cloned)
		bytes, err := patch.Data(t)
		if err != nil {
			return err
		}

		_, err = client.KubeClient().AppsV1().ReplicaSets(t.Namespace).Patch(ctx, t.Name, patch.Type(), bytes, metav1.PatchOptions{})
		if err != nil {
			return err
		}

		return nil
	case *appsv1.Deployment:
		if t.Annotations == nil {
			t.Annotations = map[string]string{}
		}

		replicas := 1
		if t.Spec.Replicas != nil {
			replicas = int(*t.Spec.Replicas)
		}

		if replicas == 0 {
			return nil
		}

		t.Annotations[ReplicasAnnotation] = strconv.Itoa(replicas)
		t.Spec.Replicas = ptr.Int32(0)
		patch := MergeFrom(cloned)
		bytes, err := patch.Data(t)
		if err != nil {
			return err
		}

		_, err = client.KubeClient().AppsV1().Deployments(t.Namespace).Patch(ctx, t.Name, patch.Type(), bytes, metav1.PatchOptions{})
		if err != nil {
			return err
		}

		return nil
	case *appsv1.StatefulSet:
		if t.Annotations == nil {
			t.Annotations = map[string]string{}
		}

		replicas := 1
		if t.Spec.Replicas != nil {
			replicas = int(*t.Spec.Replicas)
		}

		if replicas == 0 {
			return nil
		}

		t.Annotations[ReplicasAnnotation] = strconv.Itoa(replicas)
		t.Spec.Replicas = ptr.Int32(0)
		patch := MergeFrom(cloned)
		bytes, err := patch.Data(t)
		if err != nil {
			return err
		}

		_, err = client.KubeClient().AppsV1().StatefulSets(t.Namespace).Patch(ctx, t.Name, patch.Type(), bytes, metav1.PatchOptions{})
		if err != nil {
			return err
		}

		return nil
	}

	return fmt.Errorf("unrecognized object")
}

func convertFromInterface(inter map[interface{}]interface{}) []byte {
	out, err := yaml.Marshal(inter)
	if err != nil {
		panic(err)
	}

	retOut, err := yaml2.YAMLToJSON(out)
	if err != nil {
		panic(err)
	}

	return retOut
}

func convertToInterface(str runtime.Object) map[interface{}]interface{} {
	out, err := json.Marshal(str)
	if err != nil {
		panic(err)
	}

	ret := map[interface{}]interface{}{}
	err = yaml.Unmarshal(out, ret)
	if err != nil {
		panic(err)
	}

	return ret
}

func findSingleReplacedPod(ctx context.Context, client kubectl.Client, replacePod *latest.ReplacePod, timeout int64, log log.Logger) (*kubectl.SelectedPodContainer, error) {
	labelSelector := map[string]string{
		kubectl.ReplacedLabel: "true",
	}
	if replacePod.ImageName != "" {
		labelSelector[kubectl.ImageNameLabel] = replacePod.ImageName
	} else if len(replacePod.LabelSelector) > 0 {
		for k, v := range replacePod.LabelSelector {
			labelSelector[k] = v
		}
	} else {
		return nil, fmt.Errorf("imageName or labelSelector need to be defined")
	}

	// create selector
	targetOptions := targetselector.NewEmptyOptions().ApplyConfigParameter(labelSelector, replacePod.Namespace, replacePod.ContainerName, "")
	targetOptions.Timeout = timeout
	targetOptions.AllowPick = false
	targetOptions.WaitingStrategy = targetselector.NewUntilNotTerminatingStrategy(time.Second * 2)
	targetOptions.SkipInitContainers = true
	selected, err := targetselector.NewTargetSelector(client).SelectSingleContainer(ctx, targetOptions, log)
	if err != nil {
		if _, ok := err.(*targetselector.NotFoundErr); ok {
			return nil, nil
		}

		return nil, err
	}

	return selected, nil
}

func findSingleReplaceablePodParent(ctx context.Context, client kubectl.Client, config config.Config, dependencies []dependencytypes.Dependency, replacePod *latest.ReplacePod, log log.Logger) (*kubectl.SelectedPodContainer, runtime.Object, error) {
	var err error

	// create selector
	targetOptions := targetselector.NewEmptyOptions().ApplyConfigParameter(replacePod.LabelSelector, replacePod.Namespace, replacePod.ContainerName, "")
	targetOptions.Timeout = int64(300)
	targetOptions.AllowPick = false
	targetOptions.WaitingStrategy = targetselector.NewUntilNotTerminatingStrategy(time.Second * 2)
	targetOptions.SkipInitContainers = true
	targetOptions.ImageSelector, err = imageselector.Resolve(replacePod.ImageName, config, dependencies)
	if err != nil {
		return nil, nil, err
	}

	container, err := targetselector.NewTargetSelector(client).SelectSingleContainer(ctx, targetOptions, log)
	if err != nil {
		return nil, nil, err
	}

	parent, err := getParent(ctx, client, container.Pod)
	if err != nil {
		return nil, nil, errors.Wrap(err, "get pod parent")
	}

	return container, parent, nil
}

func getParent(ctx context.Context, client kubectl.Client, pod *corev1.Pod) (runtime.Object, error) {
	controller := metav1.GetControllerOf(pod)
	if controller == nil {
		return nil, fmt.Errorf("pod was not created by a ReplicaSet, Deployment or StatefulSet, patching only works if pod was created by one of those resources")
	}

	// replica set / deployment ?
	if controller.Kind == "ReplicaSet" {
		// try to find the replica set, we ignore the group version for now
		replicaSet, err := client.KubeClient().AppsV1().ReplicaSets(pod.Namespace).Get(ctx, controller.Name, metav1.GetOptions{})
		if err != nil {
			if kerrors.IsNotFound(err) {
				return nil, fmt.Errorf("unrecognized owning ReplicaSet %s group version: %s", controller.Name, controller.APIVersion)
			}

			return nil, err
		}

		replicaSetOwner := metav1.GetControllerOf(replicaSet)
		if replicaSetOwner == nil {
			return replicaSet, nil
		}

		// is deployment?
		if replicaSetOwner.Kind == "Deployment" {
			deployment, err := client.KubeClient().AppsV1().Deployments(pod.Namespace).Get(ctx, replicaSetOwner.Name, metav1.GetOptions{})
			if err != nil {
				if kerrors.IsNotFound(err) {
					return nil, fmt.Errorf("unrecognized owning Deployment %s group version: %s", replicaSetOwner.Name, replicaSetOwner.APIVersion)
				}

				return nil, err
			}

			// we stop here, if the Deployment is owned by something else we just ignore it for now
			return deployment, nil
		}

		return nil, fmt.Errorf("unrecognized owner of ReplicaSet %s: %s %s %s", replicaSet.Name, replicaSetOwner.Kind, replicaSetOwner.APIVersion, replicaSetOwner.Name)
	}

	// statefulset?
	if controller.Kind == "StatefulSet" {
		statefulSet, err := client.KubeClient().AppsV1().StatefulSets(pod.Namespace).Get(ctx, controller.Name, metav1.GetOptions{})
		if err != nil {
			if kerrors.IsNotFound(err) {
				return nil, fmt.Errorf("unrecognized owning StatefulSet %s group version: %s", controller.Name, controller.APIVersion)
			}

			return nil, err
		}

		return statefulSet, nil
	}

	return nil, fmt.Errorf("unrecognized owner of Pod %s: %s %s %s", pod.Name, controller.Kind, controller.APIVersion, controller.Name)
}
