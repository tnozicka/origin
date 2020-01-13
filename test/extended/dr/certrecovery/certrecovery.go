package certrecovery

import (
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/kubernetes/test/e2e/framework"

	exutil "github.com/openshift/origin/test/extended/util"
)

type objectReference struct {
			Name      string
			Namespace string

}

var _ = g.Describe("[Feature:CertRecovery][Disruptive]", func() {
	defer g.GinkgoRecover()
	g.It("should recover from expired certificates", func() {
		oc := exutil.NewCLI("cert-recovery", exutil.KubeConfigPath())
		namespace := oc.Namespace()

		var endpoints = []string{""}
		var certificates = []objectReference{
		var components = []objectReference{
		}{
			{
				Name:      "kube-apiserver-operator",
				Namespace: "openshift-kube-apiserver-operator",
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
		o.Expect(err).NotTo(o.HaveOccurred())

		framework.Logf("forcing cert rotation")

		framework.Logf("waiting for short lived certs to be used")

		framework.Logf("restoring the rotation time")

		framework.Logf("snapshotting time")

		framework.Logf("stopping all container runtimes and pods")

		framework.Logf("waiting for certs to expire")

		framework.Logf("verifying all certs are valid, newer then the snapshot time and have the default validity")

	})
})
