package certrecovery

import (
	"context"
	"fmt"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/e2e/framework"

	exutil "github.com/openshift/origin/test/extended/util"
)

type objectReference struct {
	Name string
}

var _ = g.Describe("[Feature:CertRecovery][Disruptive]", func() {
	defer g.GinkgoRecover()
	g.It("should recover from expired certificates", func() {
		oc := exutil.NewCLI("cert-recovery", exutil.KubeConfigPath())
		namespace := oc.Namespace()

		// var endpoints := []string{""}
		// var secrets := []objectReference{
		// }
		const ccName = "cluster"
		var clusterControllers = []schema.GroupVersionResource{
			{
				Group:    "operator",
				Version:  "v1",
				Resource: "kubeapiserver",
			},
		}

		framework.Logf("shortening the rotation time")
		certRotationConfig := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unsupported-cert-rotation-config",
				Namespace: "openshift-config",
			},
			Data: map[string]string{
				"base": "30s",
			},
		}
		certRotationConfig, err := oc.KubeClient().CoreV1().ConfigMaps(namespace).Create(certRotationConfig)
		if apierrors.IsAlreadyExists(err) {
			// update
		} else {
			o.Expect(err).NotTo(o.HaveOccurred())
		}

		framework.Logf("restarting controllers to pick up the new value")
		now := time.Now()

		// First force all to redeploy
		for _, c := range clusterControllers {
			_, err := oc.AdminDynamicClient().Resource(c).Patch(ccName, types.StrategicMergePatchType, []byte(fmt.Sprintf(`{"spec": {"forceRedeploymentReason": "%v: cert recovery e2e restarting controller to pickup the shortened rotation scale"}}`, now)), metav1.PatchOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
		}

		// then wait
		for _, c := range clusterControllers {
			framework.Logf("  waiting for controller %q to be updated", c.GroupResource())
			_, err = UntilWithSyncForDynamicExistingSingleObject(context.TODO(), oc.AdminDynamicClient().Resource(c), c.GroupResource(), metav1.NamespaceNone, ccName, IsStaticPodOperatorUpdated)
			o.Expect(err).NotTo(o.HaveOccurred())
		}

		framework.Logf("forcing cert rotation")
		// No cert can be older then this timestamps
		rotationStart := time.Now()

		framework.Logf("waiting for new (short lived) certs to be used")

		framework.Logf("restoring the rotation time")

		framework.Logf("snapshotting time")

		framework.Logf("stopping all container runtimes and pods")

		framework.Logf("waiting for certs to expire")

		framework.Logf("verifying all certs are valid, newer then the snapshot time and have the default validity")

	})
})
