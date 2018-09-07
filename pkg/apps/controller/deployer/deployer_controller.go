package deployment

import (
	"fmt"
	"reflect"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	kcorelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"

	appsv1 "github.com/openshift/api/apps/v1"
	appsutil "github.com/openshift/origin/pkg/apps/util"
	"github.com/openshift/origin/pkg/util"
)

const (
	// maxRetryCount is the maximum number of times the controller will retry errors.
	// The first requeue is after 5ms and subsequent requeues grow exponentially.
	// This effectively can extend up to 5*2^14ms which caps to 82s:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetryCount = 15

	// maxInjectedEnvironmentAllowedSize represents maximum size of a value of environment variable
	// that we will inject to a container. The default is 128Kb.
	maxInjectedEnvironmentAllowedSize = 1000 * 128
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = appsv1.GroupVersion.WithKind("ReplicationController")

// DeploymentController starts a deployment by creating a deployer pod which
// implements a deployment strategy. The status of the deployment will follow
// the status of the deployer pod. The deployer pod is correlated to the
// deployment with annotations.
//
// When the deployment enters a terminal status:
//
//   1. If the deployment finished normally, the deployer pod is deleted.
//   2. If the deployment failed, the deployer pod is not deleted.
type DeploymentController struct {
	kubeClient kubernetes.Interface

	// queue contains replication controllers that need to be synced.
	queue workqueue.RateLimitingInterface

	// rcLister can list/get replication controllers from a shared informer's cache
	rcLister kcorelisters.ReplicationControllerLister
	// rcListerSynced makes sure the rc store is synced before reconcling any deployment.
	rcListerSynced cache.InformerSynced
	// podLister can list/get pods from a shared informer's cache
	podLister kcorelisters.PodLister
	// podListerSynced makes sure the pod store is synced before reconcling any deployment.
	podListerSynced cache.InformerSynced

	// deployerImage specifies which Docker image can support the default strategies.
	deployerImage string
	// serviceAccount to create deployment pods with.
	serviceAccount string
	// environment is a set of environment variables which should be injected into all
	// deployer pod containers.
	environment []corev1.EnvVar
	// recorder is used to record events.
	recorder record.EventRecorder
}

func (c *DeploymentController) makeStateTransitionForCancelled(deployment *corev1.ReplicationController, deployerPod *corev1.Pod, dcReadOnly *appsv1.DeploymentConfig, updatedAnnotations map[string]string) (appsv1.DeploymentStatus, error) {
	deployerPodFound := deployerPod != nil
	deployerPodName := appsutil.DeployerPodNameFor(deployment)
	//deployerFinished := deployerPodFound && appsutil.IsDeployerTerminated(deployerPod)
	currentState := appsutil.DeploymentStatusFor(deployment)

	switch currentState {
	case appsv1.DeploymentStatusNew, appsv1.DeploymentStatusRetrying:
		if deployerPodFound {
			return appsv1.DeploymentStatusPending, nil
		}

		// We need to do  a live GET to find out if the deployer was created and the informers are just not caught up
		_, err := c.kubeClient.CoreV1().Pods(deployment.Namespace).Get(deployerPodName, metav1.GetOptions{})
		if err == nil {
			// We have already created the deployer pod, need to wait for informers to catch up
			return appsv1.DeploymentStatusNew, nil
		}
		if kerrors.IsNotFound(err) {
			// We didn't create the deplyoer pod so we can just cancel
			return appsv1.DeploymentStatusFailed, nil
		}

		return "", fmt.Errorf("failed to get deployer pod %s/%s: %v", deployment.Namespace, deployerPodName, err)

	case appsv1.DeploymentStatusPending, appsv1.DeploymentStatusRunning:
		// If the deployer is already deleted we wait till the pod gets removed from etcd,
		// causing a transition to a terminal state
		if deployerPodFound && deployerPod.DeletionTimestamp != nil {
			return currentState, nil
		}

		// FIXME: wrap with deletion expectations
		propagationPolicyForeground := metav1.DeletePropagationForeground

		err := c.kubeClient.CoreV1().Pods(deployment.Namespace).Delete(deployerPodName, &metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{
				UID: &deployerPod.UID,
			},
			// We need to wait for dependant's deletion here. Like if deployer pod have already created
			// some hooks we want to be sure those are deleted (and stopped) too.
			PropagationPolicy: &propagationPolicyForeground,
		})
		if err == nil {
			c.emitDeploymentEvent(deployment, corev1.EventTypeNormal, "RolloutCancelled", fmt.Sprintf("Rollout for %q cancelled", appsutil.LabelForDeployment(deployment)))
			// We need to wait for our informers to see the change
			return currentState, nil
		}
		if kerrors.IsNotFound(err) {
			// We need to wait for our informers to catch up
			return appsv1.DeploymentStatusCanceling, nil
			return currentState, nil
		}
		return "", fmt.Errorf("failed to delete deployer pod %s/%s: %v", deployment.Namespace, deployerPodName, err)

	case appsv1.DeploymentStatusCancelling:

	case appsv1.DeploymentStatusFailed, appsv1.DeploymentStatusComplete:
		delete(updatedAnnotations, appsv1.DeploymentCancelledAnnotation)
		delete(updatedAnnotations, appsv1.DeploymentStatusReasonAnnotation)

		return currentState, nil

	default:
		return "", fmt.Errorf("unexpected deployment state: %q", currentState)
	}
}

func (c *DeploymentController) makeStateTransitionRegular(deployment *corev1.ReplicationController, deployerPod *corev1.Pod, dcReadOnly *appsv1.DeploymentConfig, updatedAnnotations map[string]string) (appsv1.DeploymentStatus, error) {
	deployerPodFound := deployerPod != nil
	currentState := appsutil.DeploymentStatusFor(deployment)

	switch currentState {
	case appsv1.DeploymentStatusNew:
		if _, ok := deployment.Annotations[appsutil.DeploymentIgnorePodAnnotation]; ok {
			return appsv1.DeploymentStatusNew, nil
		}

		if appsutil.RolloutExceededTimeoutSeconds(dcReadOnly, deployment) {
			updatedAnnotations[appsv1.DeploymentStatusReasonAnnotation] = appsutil.DeploymentFailedUnableToCreateDeployerPod
			c.emitDeploymentEvent(deployment, corev1.EventTypeWarning, "RolloutTimeout",
				fmt.Sprintf("Rollout for %s/%s failed to create deployer pod (timeoutSeconds: %ds)", deployment.Namespace, deployment.Name, appsutil.GetTimeoutSecondsForStrategy(dcReadOnly)))

			return appsv1.DeploymentStatusFailed, nil
		}

		if deployerPodFound {
			return appsutil.MapDeployerPhaseToDeploymentPhaseOrStay(deployerPod, currentState)
		}

		// Generate a deployer pod.
		generatedDeployerPod := c.makeDeployerPod(deployment, dcReadOnly)

		// TODO: wrap by creation expectation
		var err error
		deployerPod, err = c.kubeClient.CoreV1().Pods(deployment.Namespace).Create(generatedDeployerPod)
		if err != nil {
			// if we cannot create a deployment pod (i.e lack of quota), match normal replica set experience and emit an event.
			c.emitDeploymentEvent(deployment, corev1.EventTypeWarning, "FailedCreate", fmt.Sprintf("Error creating deployer pod: %v", err))

			return "", fmt.Errorf("couldn't create deployer pod for %q: %v", appsutil.LabelForDeployment(deployment), err)
		}

		glog.V(4).Infof("Created deployer pod %q for %q", deployerPod.Name, appsutil.LabelForDeployment(deployment))

		// We need to stay in New phase until our informers see the pod being created
		// otherwise the following state might fail prematurely.
		return appsv1.DeploymentStatusNew, nil

	case appsv1.DeploymentStatusPending, appsv1.DeploymentStatusRunning:
		if !deployerPodFound {
			return appsv1.DeploymentStatusFailed, nil
		}

		return appsutil.MapDeployerPhaseToDeploymentPhaseOrStay(deployerPod, currentState)

	case appsv1.DeploymentStatusComplete, appsv1.DeploymentStatusFailed:
		return currentState, nil

	case appsv1.DeploymentStatusRetrying:
		if !deployerPodFound {
			delete(updatedAnnotations, appsv1.DeploymentStatusReasonAnnotation)
			delete(updatedAnnotations, appsv1.DeploymentCancelledAnnotation)
			return appsv1.DeploymentStatusNew, nil
		}

		// TODO: protect by deletion expectation
		// Delete dependants first so we are sure e.g. hooks are already deleted
		foregroundPropagation := metav1.DeletePropagationForeground
		err := c.kubeClient.CoreV1().Pods(deployerPod.Namespace).Delete(deployerPod.Name, &metav1.DeleteOptions{
			PropagationPolicy: &foregroundPropagation,
		})
		if err != nil && !kerrors.IsNotFound(err) {
			return "", nil
		}

		// We need to stay in Retrying phase until our informers see the pod being created
		// otherwise the following state might fail prematurely. It will automatically move
		// the state when we manage to delete the old Pod.
		return appsv1.DeploymentStatusRetrying, nil

	default:
		return "", fmt.Errorf("unexpected deployment state: %q", currentState)
	}
}

// handle processes a deployment and either creates a deployer pod or responds
// to a terminal deployment status. Since this controller started using caches,
// the provided rc MUST be deep-copied beforehand (see work() in deployer_factory.go).
func (c *DeploymentController) handle(deploymentReadOnly *corev1.ReplicationController, willBeDropped bool) error {
	deployerPodName := appsutil.DeployerPodNameForDeployment(deploymentReadOnly.Name)
	deployerPod, err := c.podLister.Pods(deploymentReadOnly.Namespace).Get(deployerPodName)
	switch {
	case err == nil:
		// If any adoptions are attempted, we should first recheck for deletion with
		// an uncached quorum read sometime after listing ReplicaSets (see #42639).
		canAdoptFunc := controller.RecheckDeletionTimestamp(func() (metav1.Object, error) {
			fresh, err := c.kubeClient.CoreV1().ReplicationControllers(deploymentReadOnly.Namespace).Get(deploymentReadOnly.Name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			if fresh.UID != deploymentReadOnly.UID {
				return nil, fmt.Errorf("original Deployment %s/%s is gone: got uid %s, wanted %s", deploymentReadOnly.Namespace, deploymentReadOnly.Name, fresh.UID, deploymentReadOnly.UID)
			}
			return fresh, nil
		})

		cm := controller.NewPodControllerRefManager(
			controller.RealPodControl{KubeClient: c.kubeClient, Recorder: c.recorder},
			deploymentReadOnly,
			appsutil.DeployerPodSelector(deploymentReadOnly.Name),
			controllerKind,
			canAdoptFunc,
		)
		claimedPods, err := cm.ClaimPods([]*corev1.Pod{deployerPod})
		if err != nil {
			return err
		}
		if len(claimedPods) != 1 {
			return fmt.Errorf("internal error: expected to claim 1 pod, got %d", len(claimedPods))
		}
		deployerPod = claimedPods[0]

	case kerrors.IsNotFound(err):
		deployerPod = nil

	default:
		return fmt.Errorf("failed to get deployer pod %s/%s: %v", deploymentReadOnly.Namespace, deployerPodName, err)
	}

	dc, err := appsutil.DecodeDeploymentConfig(deploymentReadOnly)
	if err != nil {
		return err
	}

	// Copy all the annotations from the deployment.
	updatedAnnotations := make(map[string]string)
	for key, value := range deploymentReadOnly.Annotations {
		updatedAnnotations[key] = value
	}

	currentState := appsutil.DeploymentStatusFor(deploymentReadOnly)

	nextState := currentState
	if appsutil.IsDeploymentCancelled(deploymentReadOnly) {
		nextState, err = c.makeStateTransitionForCancelled(deploymentReadOnly, deployerPod, dc, updatedAnnotations)
		if err != nil {
			return fmt.Errorf("failed to make state transition for cancelled deployment %s/%s: %v", deploymentReadOnly.Namespace, deploymentReadOnly.Name, err)
		}
	} else {
		nextState, err = c.makeStateTransitionRegular(deploymentReadOnly, deployerPod, dc, updatedAnnotations)
		if err != nil {
			return fmt.Errorf("failed to make state transition for regular deployment %s/%s: %v", deploymentReadOnly.Namespace, deploymentReadOnly.Name, err)
		}
	}

	updatedAnnotations[appsv1.DeploymentPodAnnotation] = deployerPod.Name
	// TODO: remove; this is not needed since we keep the deployer pods
	updatedAnnotations[appsv1.DeployerPodCreatedAtAnnotation] = deployerPod.CreationTimestamp.String()
	if deployerPod.Status.StartTime != nil {
		updatedAnnotations[appsv1.DeployerPodStartedAtAnnotation] = deployerPod.Status.StartTime.String()
	}

	updatedAnnotations[appsv1.DeploymentStatusAnnotation] = string(nextState)

	if !reflect.DeepEqual(updatedAnnotations, deploymentReadOnly.Annotations) {
		deployment := deploymentReadOnly.DeepCopy()
		deployment.Annotations = updatedAnnotations

		// If we are going to transition to failed or complete and scale is non-zero, we'll check one more
		// time to see if we are a test deployment to guarantee that we maintain the test invariant.
		if *deployment.Spec.Replicas != 0 && appsutil.IsTerminatedDeployment(deployment) {
			if config, err := appsutil.DecodeDeploymentConfig(deployment); err == nil && config.Spec.Test {
				zero := int32(0)
				deployment.Spec.Replicas = &zero
			}
		}

		if _, err := c.kubeClient.CoreV1().ReplicationControllers(deployment.Namespace).Update(deployment); err != nil {
			return fmt.Errorf("couldn't update rollout state for deployment %q to %s: %v", appsutil.LabelForDeployment(deployment), nextState, err)
		}
		glog.V(4).Infof("Updated rollout state for deployment %q from %s to %s (scale: %d)", appsutil.LabelForDeployment(deployment), currentState, nextState, *deployment.Spec.Replicas)
	}

	return nil
}

// getPodTerminatedTimestamp gets the first terminated container in a pod and
// return its termination timestamp.
func getPodTerminatedTimestamp(pod *corev1.Pod) *metav1.Time {
	for _, c := range pod.Status.ContainerStatuses {
		if t := c.State.Terminated; t != nil {
			return &t.FinishedAt
		}
	}
	return nil
}

// makeDeployerPod creates a pod which implements deployment behavior. The pod is correlated to
// the deployment with an annotation.
func (c *DeploymentController) makeDeployerPod(deployment *corev1.ReplicationController, dcReadOnly *appsv1.DeploymentConfig) *corev1.Pod {
	dc := dcReadOnly.DeepCopy()
	container := c.makeDeployerContainer(&dc.Spec.Strategy)

	// Add deployment environment variables to the container.
	envVars := []corev1.EnvVar{}
	for _, env := range container.Env {
		envVars = append(envVars, env)
	}
	envVars = append(envVars, corev1.EnvVar{Name: "OPENSHIFT_DEPLOYMENT_NAME", Value: deployment.Name})
	envVars = append(envVars, corev1.EnvVar{Name: "OPENSHIFT_DEPLOYMENT_NAMESPACE", Value: deployment.Namespace})

	// Assigning to a variable since its address is required
	maxDeploymentDurationSeconds := appsutil.MaxDeploymentDurationSeconds
	if dc.Spec.Strategy.ActiveDeadlineSeconds != nil {
		maxDeploymentDurationSeconds = *(dc.Spec.Strategy.ActiveDeadlineSeconds)
	}

	gracePeriod := int64(10)
	shareProcessNamespace := false

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: appsutil.DeployerPodNameForDeployment(deployment.Name),
			Annotations: map[string]string{
				appsv1.DeploymentAnnotation:       deployment.Name,
				appsv1.DeploymentConfigAnnotation: appsutil.DeploymentConfigNameFor(deployment),
			},
			Labels: map[string]string{
				appsv1.DeployerPodForDeploymentLabel: deployment.Name,
			},
			// Set the owner reference to current deployment, so in case the deployment fails
			// and the deployer pod is preserved when a revisionHistory limit is reached and the
			// deployment is removed, we also remove the deployer pod with it.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ReplicationController",
				Name:       deployment.Name,
				UID:        deployment.UID,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:      "deployment",
					Command:   container.Command,
					Args:      container.Args,
					Image:     container.Image,
					Env:       envVars,
					Resources: dc.Spec.Strategy.Resources,
				},
			},
			ActiveDeadlineSeconds: &maxDeploymentDurationSeconds,
			DNSPolicy:             deployment.Spec.Template.Spec.DNSPolicy,
			ImagePullSecrets:      deployment.Spec.Template.Spec.ImagePullSecrets,
			Tolerations:           deployment.Spec.Template.Spec.Tolerations,
			// Setting the node selector on the deployer pod so that it is created
			// on the same set of nodes as the pods.
			NodeSelector:                  deployment.Spec.Template.Spec.NodeSelector,
			RestartPolicy:                 corev1.RestartPolicyNever,
			ServiceAccountName:            c.serviceAccount,
			TerminationGracePeriodSeconds: &gracePeriod,
			ShareProcessNamespace:         &shareProcessNamespace,
		},
	}

	// MergeInfo will not overwrite values unless the flag OverwriteExistingDstKey is set.
	util.MergeInto(pod.Labels, dc.Spec.Strategy.Labels, 0)
	util.MergeInto(pod.Annotations, dc.Spec.Strategy.Annotations, 0)

	pod.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent

	return pod
}

// makeDeployerContainer creates containers in the following way:
//
//   1. For the Recreate and Rolling strategies, strategy, use the factory's
//      DeployerImage as the container image, and the factory's Environment
//      as the container environment.
//   2. For all Custom strategies, or if the CustomParams field is set, use
//      the strategy's image for the container image, and use the combination
//      of the factory's Environment and the strategy's environment as the
//      container environment.
//
func (c *DeploymentController) makeDeployerContainer(strategy *appsv1.DeploymentStrategy) *corev1.Container {
	image := c.deployerImage
	var environment []corev1.EnvVar
	var command []string

	set := sets.NewString()
	// Use user-defined values from the strategy input.
	if p := strategy.CustomParams; p != nil {
		if len(p.Image) > 0 {
			image = p.Image
		}
		if len(p.Command) > 0 {
			command = p.Command
		}
		for _, env := range strategy.CustomParams.Environment {
			set.Insert(env.Name)
			environment = append(environment, env)
		}
	}

	// Set default environment values
	for _, env := range c.environment {
		if set.Has(env.Name) {
			continue
		}
		// TODO: The size of environment value should be probably validated in k8s api validation
		//       as when the env var size is more than 128kb the execve calls will fail.
		if len(env.Value) > maxInjectedEnvironmentAllowedSize {
			glog.Errorf("failed to inject %s environment variable as the size exceed %d bytes", env.Name, maxInjectedEnvironmentAllowedSize)
			continue
		}
		environment = append(environment, env)
	}

	return &corev1.Container{
		Image:   image,
		Command: command,
		Env:     environment,
	}
}

func (c *DeploymentController) emitDeploymentEvent(deployment *corev1.ReplicationController, eventType, title, message string) {
	if config, _ := appsutil.DecodeDeploymentConfig(deployment); config != nil {
		c.recorder.Eventf(config, eventType, title, message)
	} else {
		c.recorder.Eventf(deployment, eventType, title, message)
	}
}

func (c *DeploymentController) handleErr(err error, key interface{}, deployment *corev1.ReplicationController) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if c.queue.NumRequeues(key) < maxRetryCount {
		glog.Infof("Error syncing deployment %v: %v", key, err)

		c.queue.AddRateLimited(key)
		return
	}

	glog.Warningf("Stopped retrying: %v", err)
	c.queue.Forget(key)
}
