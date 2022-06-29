// Copyright (c) 2020, 2022 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package intrusiondetection

import (
	"context"
	"fmt"
	"time"

	"github.com/tigera/operator/pkg/controller/certificatemanager"
	rtest "github.com/tigera/operator/pkg/render/common/test"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tigera/operator/pkg/render/intrusiondetection/dpi"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/components"
	"github.com/tigera/operator/test"

	v3 "github.com/tigera/api/pkg/apis/projectcalico/v3"
	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/apis"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/render"
	"github.com/tigera/operator/pkg/render/common/cloudconfig"
	relasticsearch "github.com/tigera/operator/pkg/render/common/elasticsearch"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("IntrusionDetection controller tests", func() {
	var c client.Client
	var ctx context.Context
	var r ReconcileIntrusionDetection
	var scheme *runtime.Scheme
	var mockStatus *status.MockStatus

	BeforeEach(func() {
		// The schema contains all objects that should be known to the fake client when the test runs.
		scheme = runtime.NewScheme()
		Expect(apis.AddToScheme(scheme)).NotTo(HaveOccurred())
		Expect(appsv1.SchemeBuilder.AddToScheme(scheme)).ShouldNot(HaveOccurred())
		Expect(rbacv1.SchemeBuilder.AddToScheme(scheme)).ShouldNot(HaveOccurred())
		Expect(batchv1.SchemeBuilder.AddToScheme(scheme)).ShouldNot(HaveOccurred())
		Expect(operatorv1.SchemeBuilder.AddToScheme(scheme)).NotTo(HaveOccurred())

		// Create a client that will have a crud interface of k8s objects.
		c = fake.NewClientBuilder().WithScheme(scheme).Build()
		ctx = context.Background()

		// Create an object we can use throughout the test to do the compliance reconcile loops.
		mockStatus = &status.MockStatus{}
		mockStatus.On("AddDaemonsets", mock.Anything).Return()
		mockStatus.On("AddDeployments", mock.Anything).Return()
		mockStatus.On("RemoveDeployments", mock.Anything).Return()
		mockStatus.On("AddStatefulSets", mock.Anything).Return()
		mockStatus.On("AddCronJobs", mock.Anything)
		mockStatus.On("IsAvailable").Return(true)
		mockStatus.On("OnCRFound").Return()
		mockStatus.On("ClearDegraded")
		mockStatus.On("SetDegraded", "Waiting for LicenseKeyAPI to be ready", "").Return().Maybe()
		mockStatus.On("ReadyToMonitor")

		cloudConfig := cloudconfig.NewCloudConfig("id", "tenantName", "externalES.com", "externalKB.com", false)
		Expect(c.Create(ctx, cloudConfig.ConfigMap())).ToNot(HaveOccurred())

		// Create an object we can use throughout the test to do the compliance reconcile loops.
		// As the parameters in the client changes, we expect the outcomes of the reconcile loops to change.
		r = ReconcileIntrusionDetection{
			client:          c,
			scheme:          scheme,
			provider:        operatorv1.ProviderNone,
			status:          mockStatus,
			licenseAPIReady: &utils.ReadyFlag{},
			dpiAPIReady:     &utils.ReadyFlag{},
			elasticExternal: false,
		}

		// We start off with a 'standard' installation, with nothing special
		Expect(c.Create(
			ctx,
			&operatorv1.Installation{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: operatorv1.InstallationSpec{
					Variant:  operatorv1.TigeraSecureEnterprise,
					Registry: "some.registry.org/",
				},
				Status: operatorv1.InstallationStatus{
					Variant: operatorv1.TigeraSecureEnterprise,
					Computed: &operatorv1.InstallationSpec{
						Registry: "my-reg",
						// The test is provider agnostic.
						KubernetesProvider: operatorv1.ProviderNone,
					},
				},
			})).NotTo(HaveOccurred())

		// The compliance reconcile loop depends on a ton of objects that should be available in your client as
		// prerequisites. Without them, compliance will not even start creating objects. Let's create them now.
		Expect(c.Create(ctx, &operatorv1.APIServer{
			ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"},
			Status:     operatorv1.APIServerStatus{State: operatorv1.TigeraStatusReady},
		})).NotTo(HaveOccurred())
		Expect(c.Create(ctx, &v3.LicenseKey{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Status:     v3.LicenseKeyStatus{Features: []string{common.ThreatDefenseFeature}}})).NotTo(HaveOccurred())
		Expect(c.Create(ctx, &operatorv1.LogCollector{
			ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"}})).NotTo(HaveOccurred())

		certificateManager, err := certificatemanager.Create(c, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Create(ctx, certificateManager.KeyPair().Secret(common.OperatorNamespace()))) // Persist the root-ca in the operator namespace.
		kiibanaTLS, err := certificateManager.GetOrCreateKeyPair(c, relasticsearch.PublicCertSecret, common.OperatorNamespace(), []string{relasticsearch.PublicCertSecret})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Create(ctx, kiibanaTLS.Secret(common.OperatorNamespace()))).NotTo(HaveOccurred())

		Expect(c.Create(ctx, relasticsearch.NewClusterConfig("cluster", 1, 1, 1).ConfigMap())).NotTo(HaveOccurred())
		Expect(c.Create(ctx, rtest.CreateCertSecret(render.ElasticsearchIntrusionDetectionUserSecret, common.OperatorNamespace(), render.GuardianSecretName)))
		Expect(c.Create(ctx, rtest.CreateCertSecret(render.ElasticsearchADJobUserSecret, common.OperatorNamespace(), render.GuardianSecretName)))
		Expect(c.Create(ctx, rtest.CreateCertSecret(render.ElasticsearchPerformanceHotspotsUserSecret, common.OperatorNamespace(), render.GuardianSecretName)))
		Expect(c.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      render.ECKLicenseConfigMapName,
				Namespace: render.ECKOperatorNamespace,
			},
			Data: map[string]string{"eck_license_level": string(render.ElasticsearchLicenseTypeEnterpriseTrial)},
		})).NotTo(HaveOccurred())

		Expect(c.Create(ctx, &v3.DeepPacketInspection{ObjectMeta: metav1.ObjectMeta{Name: "test-dpi", Namespace: "test-dpi-ns"}})).ShouldNot(HaveOccurred())

		// Apply the intrusiondetection CR to the fake cluster.
		Expect(c.Create(ctx, &operatorv1.IntrusionDetection{ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"}})).NotTo(HaveOccurred())

		// mark that the watch for license key and dpi was successful
		r.licenseAPIReady.MarkAsReady()
		r.dpiAPIReady.MarkAsReady()
	})

	Context("image reconciliation", func() {
		BeforeEach(func() {
			Expect(c.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      render.ElasticsearchIntrusionDetectionJobUserSecret,
					Namespace: "tigera-operator"}})).NotTo(HaveOccurred())
		})

		It("should use builtin images", func() {
			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).ShouldNot(HaveOccurred())

			d := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "intrusion-detection-controller",
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &d)).To(BeNil())
			Expect(d.Spec.Template.Spec.Containers).To(HaveLen(1))
			controller := test.GetContainer(d.Spec.Template.Spec.Containers, "controller")
			Expect(controller).ToNot(BeNil())
			Expect(controller.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s:tesla-%s",
					components.ComponentIntrusionDetectionController.Image,
					components.ComponentIntrusionDetectionController.Version)))

			j := batchv1.Job{
				TypeMeta: metav1.TypeMeta{Kind: "Job", APIVersion: "batch/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      render.IntrusionDetectionInstallerJobName,
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &j)).To(BeNil())
			Expect(j.Spec.Template.Spec.Containers).To(HaveLen(1))
			installer := test.GetContainer(j.Spec.Template.Spec.Containers, "elasticsearch-job-installer")
			Expect(installer).ToNot(BeNil())
			Expect(installer.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s:%s",
					components.ComponentElasticTseeInstaller.Image,
					components.ComponentElasticTseeInstaller.Version)))

			training_pt := corev1.PodTemplate{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodTemplate",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: render.IntrusionDetectionNamespace,
					Name:      render.ADJobPodTemplateBaseName + ".training",
				},
			}
			Expect(test.GetResource(c, &training_pt)).To(BeNil())
			Expect(training_pt.Template.Spec.Containers).To(HaveLen(1))
			adjobs_training := test.GetContainer(training_pt.Template.Spec.Containers, "adjobs")
			Expect(adjobs_training).ToNot(BeNil())
			Expect(adjobs_training.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s:%s",
					components.ComponentAnomalyDetectionJobs.Image,
					components.ComponentAnomalyDetectionJobs.Version)))

			detection_pt := corev1.PodTemplate{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodTemplate",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: render.IntrusionDetectionNamespace,
					Name:      render.ADJobPodTemplateBaseName + ".detection",
				},
			}
			Expect(test.GetResource(c, &detection_pt)).To(BeNil())
			Expect(detection_pt.Template.Spec.Containers).To(HaveLen(1))
			adjobs_detection := test.GetContainer(detection_pt.Template.Spec.Containers, "adjobs")
			Expect(adjobs_detection).ToNot(BeNil())
			Expect(adjobs_detection.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s:%s",
					components.ComponentAnomalyDetectionJobs.Image,
					components.ComponentAnomalyDetectionJobs.Version)))

			adAPI := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "anomaly-detection-api",
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &adAPI)).To(BeNil())
			Expect(adAPI.Spec.Template.Spec.Containers).To(HaveLen(1))
			adAPIContainer := test.GetContainer(adAPI.Spec.Template.Spec.Containers, "anomaly-detection-api")
			Expect(adAPIContainer).ToNot(BeNil())
			Expect(adAPIContainer.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s:%s",
					components.ComponentAnomalyDetectionAPI.Image,
					components.ComponentAnomalyDetectionAPI.Version)))

		})
		It("should use images from imageset", func() {
			Expect(c.Create(ctx, &operatorv1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "enterprise-" + components.EnterpriseRelease},
				Spec: operatorv1.ImageSetSpec{
					Images: []operatorv1.Image{
						{Image: "tigera/intrusion-detection-job-installer", Digest: "sha256:intrusiondetectionjobinstallerhash"},
						{Image: "tigera/intrusion-detection-controller", Digest: "sha256:intrusiondetectioncontrollerhash"},
						{Image: "tigera/deep-packet-inspection", Digest: "sha256:deeppacketinspectionhash"},
						{Image: "tigera/anomaly_detection_jobs", Digest: "sha256:anomalydetectionjobs"},
						{Image: "tigera/anomaly-detection-api", Digest: "sha256:anomalydetectionapi"},
					},
				},
			})).ToNot(HaveOccurred())

			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).ShouldNot(HaveOccurred())

			d := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "intrusion-detection-controller",
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &d)).To(BeNil())
			Expect(d.Spec.Template.Spec.Containers).To(HaveLen(1))
			controller := test.GetContainer(d.Spec.Template.Spec.Containers, "controller")
			Expect(controller).ToNot(BeNil())
			Expect(controller.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s@%s",
					components.ComponentIntrusionDetectionController.Image,
					"sha256:intrusiondetectioncontrollerhash")))

			j := batchv1.Job{
				TypeMeta: metav1.TypeMeta{Kind: "Job", APIVersion: "batch/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      render.IntrusionDetectionInstallerJobName,
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &j)).To(BeNil())
			Expect(j.Spec.Template.Spec.Containers).To(HaveLen(1))
			installer := test.GetContainer(j.Spec.Template.Spec.Containers, "elasticsearch-job-installer")
			Expect(installer).ToNot(BeNil())
			Expect(installer.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s@%s",
					components.ComponentElasticTseeInstaller.Image,
					"sha256:intrusiondetectionjobinstallerhash")))

			ds := appsv1.DaemonSet{
				TypeMeta: metav1.TypeMeta{Kind: "DaemonSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      dpi.DeepPacketInspectionName,
					Namespace: dpi.DeepPacketInspectionNamespace,
				}}
			Expect(test.GetResource(c, &ds)).To(BeNil())
			Expect(ds.Spec.Template.Spec.Containers).To(HaveLen(1))
			dpiContainer := test.GetContainer(ds.Spec.Template.Spec.Containers, dpi.DeepPacketInspectionName)
			Expect(dpiContainer).ToNot(BeNil())
			Expect(dpiContainer.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s@%s",
					components.ComponentDeepPacketInspection.Image,
					"sha256:deeppacketinspectionhash")))

			training_pt := corev1.PodTemplate{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodTemplate",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: render.IntrusionDetectionNamespace,
					Name:      render.ADJobPodTemplateBaseName + ".training",
				},
			}
			Expect(test.GetResource(c, &training_pt)).To(BeNil())
			Expect(training_pt.Template.Spec.Containers).To(HaveLen(1))
			adjobs_training := test.GetContainer(training_pt.Template.Spec.Containers, "adjobs")
			Expect(adjobs_training).ToNot(BeNil())
			Expect(adjobs_training.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s@%s",
					components.ComponentAnomalyDetectionJobs.Image,
					"sha256:anomalydetectionjobs")))

			detection_pt := corev1.PodTemplate{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodTemplate",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: render.IntrusionDetectionNamespace,
					Name:      render.ADJobPodTemplateBaseName + ".detection",
				},
			}
			Expect(test.GetResource(c, &detection_pt)).To(BeNil())
			Expect(detection_pt.Template.Spec.Containers).To(HaveLen(1))
			adjobs_detection := test.GetContainer(detection_pt.Template.Spec.Containers, "adjobs")
			Expect(adjobs_detection).ToNot(BeNil())
			Expect(adjobs_detection.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s@%s",
					components.ComponentAnomalyDetectionJobs.Image,
					"sha256:anomalydetectionjobs")))

			adAPI := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "anomaly-detection-api",
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &adAPI)).To(BeNil())
			Expect(adAPI.Spec.Template.Spec.Containers).To(HaveLen(1))
			adAPIContainer := test.GetContainer(adAPI.Spec.Template.Spec.Containers, "anomaly-detection-api")
			Expect(adAPIContainer).ToNot(BeNil())
			Expect(adAPIContainer.Image).To(Equal(
				fmt.Sprintf("some.registry.org/%s@%s",
					components.ComponentAnomalyDetectionAPI.Image,
					"sha256:anomalydetectionapi")))
		})
		It("should not register intrusion-detection-job-installer image when cluster is managed", func() {
			Expect(c.Create(ctx, &operatorv1.ManagementClusterConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"},
				Spec: operatorv1.ManagementClusterConnectionSpec{
					ManagementClusterAddr: "127.0.0.1:12345",
				},
			})).ToNot(HaveOccurred())

			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).ShouldNot(HaveOccurred())

			j := batchv1.Job{
				TypeMeta: metav1.TypeMeta{Kind: "Job", APIVersion: "batch/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      render.IntrusionDetectionInstallerJobName,
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			// Shouldn't be able to find the job in a managed cluster.
			Expect(test.GetResource(c, &j)).NotTo(BeNil())
		})
		It("should register intrusion-detection-job-installer image when in a management cluster", func() {
			Expect(c.Create(ctx, &operatorv1.ManagementCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"},
				Spec: operatorv1.ManagementClusterSpec{
					Address: "127.0.0.1:12345",
				},
			})).ToNot(HaveOccurred())

			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).ShouldNot(HaveOccurred())

			j := batchv1.Job{
				TypeMeta: metav1.TypeMeta{Kind: "Job", APIVersion: "batch/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      render.IntrusionDetectionInstallerJobName,
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &j)).To(BeNil())
		})
	})

	Context("secret availability", func() {
		BeforeEach(func() {
			mockStatus.On("SetDegraded", mock.Anything, mock.Anything).Return()
		})

		It("should not wait on tigera-ee-installer-elasticsearch-access secret when cluster is managed", func() {
			Expect(c.Create(ctx, &operatorv1.ManagementClusterConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"},
				Spec: operatorv1.ManagementClusterConnectionSpec{
					ManagementClusterAddr: "127.0.0.1:12345",
				},
			})).ToNot(HaveOccurred())

			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(mockStatus.AssertNumberOfCalls(nil, "SetDegraded", 0)).To(BeTrue())
		})

		It("should wait on tigera-ee-installer-elasticsearch-access secret when in a management cluster", func() {
			Expect(c.Create(ctx, &operatorv1.ManagementCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"},
				Spec: operatorv1.ManagementClusterSpec{
					Address: "127.0.0.1:12345",
				},
			})).ToNot(HaveOccurred())

			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).ShouldNot(HaveOccurred())
			// The missing secret should force utils.ElasticSearch to return a NotFound error which triggers r.status.SetDegraded.
			Expect(mockStatus.AssertNumberOfCalls(nil, "SetDegraded", 1)).To(BeTrue())
		})
	})

	Context("Feature intrusion detection not active", func() {
		BeforeEach(func() {
			By("Deleting the previous license")
			Expect(c.Delete(ctx, &v3.LicenseKey{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Status: v3.LicenseKeyStatus{Features: []string{common.ThreatDefenseFeature}}})).NotTo(HaveOccurred())
			By("Creating a new license that does not contain intrusion detection as a feature")
			Expect(c.Create(ctx, &v3.LicenseKey{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Status: v3.LicenseKeyStatus{Features: []string{}}})).NotTo(HaveOccurred())
		})

		It("should not create resources", func() {
			mockStatus.On("SetDegraded", "Feature is not active", "License does not support this feature").Return()
			mockStatus.On("SetDegraded", "Elasticsearch secrets are not available yet, waiting until they become available", "secrets \"tigera-ee-installer-elasticsearch-access\" not found").Return().Maybe()

			result, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(0 * time.Second))

			d := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "intrusion-detection-controller",
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &d)).NotTo(BeNil())
			controller := test.GetContainer(d.Spec.Template.Spec.Containers, "controller")
			Expect(controller).To(BeNil())

			j := batchv1.Job{
				TypeMeta: metav1.TypeMeta{Kind: "Job", APIVersion: "batch/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      render.IntrusionDetectionInstallerJobName,
					Namespace: render.IntrusionDetectionNamespace,
				},
			}
			Expect(test.GetResource(c, &j)).NotTo(BeNil())
			installer := test.GetContainer(j.Spec.Template.Spec.Containers, "elasticsearch-job-installer")
			Expect(installer).To(BeNil())
		})

		AfterEach(func() {
			By("Deleting the previous license")
			Expect(c.Delete(ctx, &v3.LicenseKey{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Status: v3.LicenseKeyStatus{Features: []string{}}})).NotTo(HaveOccurred())
		})
	})

	Context("Reconcile tests", func() {
		BeforeEach(func() {
			mockStatus.On("SetDegraded", mock.Anything, mock.Anything).Return()
		})

		It("should Reconcile with default values for intrusion detection resource", func() {
			result, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(0 * time.Second))

			ids := operatorv1.IntrusionDetection{ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"}}
			Expect(test.GetResource(c, &ids)).To(BeNil())
			Expect(ids.Spec.ComponentResources).ShouldNot(BeNil())
			Expect(len(ids.Spec.ComponentResources)).Should(Equal(1))
			Expect(ids.Spec.ComponentResources[0].ComponentName).Should(Equal(operatorv1.ComponentNameDeepPacketInspection))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Requests.Cpu()).Should(Equal(resource.MustParse(dpi.DefaultCPURequest)))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Limits.Cpu()).Should(Equal(resource.MustParse(dpi.DefaultCPULimit)))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Requests.Memory()).Should(Equal(resource.MustParse(dpi.DefaultMemoryRequest)))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Limits.Memory()).Should(Equal(resource.MustParse(dpi.DefaultMemoryLimit)))
		})

		It("should not overwrite resource requirements if they are already set", func() {
			By("Deleting the previous IntrusionDetection")
			Expect(c.Delete(ctx, &operatorv1.IntrusionDetection{ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"}})).NotTo(HaveOccurred())

			memoryLimit := "5Gi"
			memoryRequest := "5Gi"
			cpuLimit := "3"
			cpuRequest := "2"

			By("Creating IntrusionDetection resource with custom resource requirements")
			Expect(c.Create(ctx, &operatorv1.IntrusionDetection{
				ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"},
				Spec: operatorv1.IntrusionDetectionSpec{
					ComponentResources: []operatorv1.IntrusionDetectionComponentResource{
						{
							ComponentName: operatorv1.ComponentNameDeepPacketInspection,
							ResourceRequirements: &corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse(memoryLimit),
									corev1.ResourceCPU:    resource.MustParse(cpuLimit),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse(memoryRequest),
									corev1.ResourceCPU:    resource.MustParse(cpuRequest),
								},
							},
						},
					},
				},
			})).
				NotTo(HaveOccurred())

			result, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(0 * time.Second))

			ids := operatorv1.IntrusionDetection{ObjectMeta: metav1.ObjectMeta{Name: "tigera-secure"}}
			Expect(test.GetResource(c, &ids)).To(BeNil())
			Expect(ids.Spec.ComponentResources).ShouldNot(BeNil())
			Expect(len(ids.Spec.ComponentResources)).Should(Equal(1))
			Expect(ids.Spec.ComponentResources[0].ComponentName).Should(Equal(operatorv1.ComponentNameDeepPacketInspection))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Requests.Cpu()).Should(Equal(resource.MustParse(cpuRequest)))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Limits.Cpu()).Should(Equal(resource.MustParse(cpuLimit)))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Requests.Memory()).Should(Equal(resource.MustParse(memoryRequest)))
			Expect(*ids.Spec.ComponentResources[0].ResourceRequirements.Limits.Memory()).Should(Equal(resource.MustParse(memoryLimit)))
		})
	})
})
