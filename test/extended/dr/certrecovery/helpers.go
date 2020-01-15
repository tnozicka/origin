package certrecovery

import (
	"context"
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// TODO: extract to kubernetes
func alreadyExistsPreconditionFunc(gr schema.GroupResource, namespace, name string) func(store cache.Store) (bool, error) {
	return func(store cache.Store) (bool, error) {
		_, exists, err := store.Get(&metav1.ObjectMeta{Namespace: namespace, Name: name})
		if err != nil {
			return true, err
		}
		if !exists {
			// We need to make sure we see the object in the cache before we start waiting for events
			// or we would be waiting for the timeout if such object didn't exist.
			return true, apierrors.NewNotFound(gr, name)
		}

		return false, nil
	}
}

func UntilWithSyncForDynamicExistingSingleObject(ctx context.Context, client dynamic.NamespaceableResourceInterface, gr schema.GroupResource, namespace, name string, conditions ...watchtools.ConditionFunc) (*watch.Event, error) {
	ccFieldSelector := fields.OneTermEqualSelector("metadata.name", name).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = ccFieldSelector
			return client.Namespace(metav1.NamespaceNone).List(options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = ccFieldSelector
			return client.Namespace(metav1.NamespaceNone).Watch(options)
		},
	}

	return watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, alreadyExistsPreconditionFunc(gr, namespace, name), conditions...)
}

func IsStaticPodOperatorUpdated(e watch.Event) (bool, error) {
	switch t := e.Type; t {
	case watch.Added, watch.Modified:
		u := e.Object.(*unstructured.Unstructured)
		status := &operatorv1.StaticPodOperatorStatus{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent()["status"].(map[string]interface{}), status)
		if err != nil {
			return true, err
		}

		if u.GetGeneration() != status.ObservedGeneration {
			return false, nil
		}

		for _, ns := range status.NodeStatuses {
			if ns.CurrentRevision != ns.TargetRevision {
				return false, nil
			}
		}

		// TODO: possibly check if the operator came up, but we mostly care just about not having any old code running

		return true, nil

	case watch.Deleted:
		// We need to abort to avoid cases of recreation and not to silently watch the wrong (new) object
		return true, fmt.Errorf("object has been deleted")

	default:
		return true, fmt.Errorf("internal error: unexpected event %#w", e)
	}
}
