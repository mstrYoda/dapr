// ------------------------------------------------------------
// Copyright (c) Microsoft Corporation and Dapr Contributors.
// Licensed under the MIT License.
// ------------------------------------------------------------

package kubernetes

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// MiniKubeIPEnvVar is the environment variable name which will have Minikube node IP
	MiniKubeIPEnvVar = "DAPR_TEST_MINIKUBE_IP"

	// ContainerLogPathEnvVar is the environment variable name which will have the container logs
	ContainerLogPathEnvVar = "DAPR_CONTAINER_LOG_PATH"

	// ContainerLogDefaultPath
	ContainerLogDefaultPath = "./container_logs"

	// PollInterval is how frequently e2e tests will poll for updates.
	PollInterval = 1 * time.Second
	// PollTimeout is how long e2e tests will wait for resource updates when polling.
	PollTimeout = 10 * time.Minute

	// maxReplicas is the maximum replicas of replica sets
	maxReplicas = 10
)

// AppManager holds Kubernetes clients and namespace used for test apps
// and provides the helpers to manage the test apps
type AppManager struct {
	client    *KubeClient
	namespace string
	app       AppDescription

	forwarder *PodPortForwarder

	logPrefix string
}

// PodInfo holds information about a given pod.
type PodInfo struct {
	Name string
	IP   string
}

// NewAppManager creates AppManager instance
func NewAppManager(kubeClients *KubeClient, namespace string, app AppDescription) *AppManager {
	return &AppManager{
		client:    kubeClients,
		namespace: namespace,
		app:       app,
	}
}

// Name returns app name
func (m *AppManager) Name() string {
	return m.app.AppName
}

// App returns app description
func (m *AppManager) App() AppDescription {
	return m.app
}

// Init installs app by AppDescription
func (m *AppManager) Init() error {
	// Get or create test namespaces
	if _, err := m.GetOrCreateNamespace(); err != nil {
		return err
	}

	// TODO: Dispose app if option is required
	if err := m.Dispose(true); err != nil {
		return err
	}

	// Deploy app and wait until deployment is done
	if _, err := m.Deploy(); err != nil {
		return err
	}

	// Wait until app is deployed completely
	if _, err := m.WaitUntilDeploymentState(m.IsDeploymentDone); err != nil {
		return err
	}

	// Validate daprd side car is injected
	if ok, err := m.ValidiateSideCar(); err != nil || ok != m.app.IngressEnabled {
		return err
	}

	// Create Ingress endpoint
	if _, err := m.CreateIngressService(); err != nil {
		return err
	}

	m.forwarder = NewPodPortForwarder(m.client, m.namespace)

	m.logPrefix = os.Getenv(ContainerLogPathEnvVar)

	if m.logPrefix == "" {
		m.logPrefix = ContainerLogDefaultPath
	}

	if err := os.MkdirAll(m.logPrefix, os.ModePerm); err != nil {
		log.Printf("Failed to create output log directory '%s' Error was: '%s'. Container logs will be discarded", m.logPrefix, err)
		m.logPrefix = ""
	}

	return nil
}

// Dispose deletes deployment and service
func (m *AppManager) Dispose(wait bool) error {
	if m.logPrefix != "" {
		if err := m.SaveContainerLogs(); err != nil {
			log.Printf("Failed to retrieve container logs for %s. Error was: %s", m.app.AppName, err)
		}
	}

	if err := m.DeleteDeployment(true); err != nil {
		return err
	}

	if err := m.DeleteService(true); err != nil {
		return err
	}

	if wait {
		if _, err := m.WaitUntilDeploymentState(m.IsDeploymentDeleted); err != nil {
			return err
		}

		if _, err := m.WaitUntilServiceState(m.IsServiceDeleted); err != nil {
			return err
		}
	}

	if m.forwarder != nil {
		m.forwarder.Close()
	}

	return nil
}

// Deploy deploys app based on app description
func (m *AppManager) Deploy() (*appsv1.Deployment, error) {
	deploymentsClient := m.client.Deployments(m.namespace)
	obj := buildDeploymentObject(m.namespace, m.app)

	result, err := deploymentsClient.Create(context.TODO(), obj, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// WaitUntilDeploymentState waits until isState returns true
func (m *AppManager) WaitUntilDeploymentState(isState func(*appsv1.Deployment, error) bool) (*appsv1.Deployment, error) {
	deploymentsClient := m.client.Deployments(m.namespace)

	var lastDeployment *appsv1.Deployment

	waitErr := wait.PollImmediate(PollInterval, PollTimeout, func() (bool, error) {
		var err error
		lastDeployment, err = deploymentsClient.Get(context.TODO(), m.app.AppName, metav1.GetOptions{})
		done := isState(lastDeployment, err)
		if !done && err != nil {
			return true, err
		}
		return done, nil
	})

	if waitErr != nil {
		return nil, fmt.Errorf("deployment %q is not in desired state, received: %+v: %s", m.app.AppName, lastDeployment, waitErr)
	}

	return lastDeployment, nil
}

// IsDeploymentDone returns true if deployment object completes pod deployments
func (m *AppManager) IsDeploymentDone(deployment *appsv1.Deployment, err error) bool {
	return err == nil && deployment.Generation == deployment.Status.ObservedGeneration && deployment.Status.ReadyReplicas == m.app.Replicas && deployment.Status.AvailableReplicas == m.app.Replicas
}

// IsDeploymentDeleted returns true if deployment does not exist or current pod replica is zero
func (m *AppManager) IsDeploymentDeleted(deployment *appsv1.Deployment, err error) bool {
	return err != nil && errors.IsNotFound(err)
}

// ValidiateSideCar validates that dapr side car is running in dapr enabled pods
func (m *AppManager) ValidiateSideCar() (bool, error) {
	if !m.app.DaprEnabled {
		return false, fmt.Errorf("dapr is not enabled for this app")
	}

	podClient := m.client.Pods(m.namespace)

	// Filter only 'testapp=appName' labeled Pods
	podList, err := podClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", TestAppLabelKey, m.app.AppName),
	})
	if err != nil {
		return false, err
	}

	if len(podList.Items) != int(m.app.Replicas) {
		return false, fmt.Errorf("expected number of pods for %s: %d, received: %d", m.app.AppName, m.app.Replicas, len(podList.Items))
	}

	// Each pod must have daprd sidecar
	for _, pod := range podList.Items {
		daprdFound := false
		for _, container := range pod.Spec.Containers {
			if container.Name == DaprSideCarName {
				daprdFound = true
			}
		}
		if !daprdFound {
			return false, fmt.Errorf("cannot find dapr sidecar in pod %s", pod.Name)
		}
	}

	return true, nil
}

// DoPortForwarding performs port forwarding for given podname to access test apps in the cluster
func (m *AppManager) DoPortForwarding(podName string, targetPorts ...int) ([]int, error) {
	podClient := m.client.Pods(m.namespace)
	// Filter only 'testapp=appName' labeled Pods
	podList, err := podClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", TestAppLabelKey, m.app.AppName),
	})

	if err != nil {
		return nil, err
	}

	name := podName

	// if given pod name is empty , pick the first matching pod name
	if name == "" {
		for _, pod := range podList.Items {
			name = pod.Name
			break
		}
	}

	return m.forwarder.Connect(name, targetPorts...)
}

// ScaleDeploymentReplica scales the deployment
func (m *AppManager) ScaleDeploymentReplica(replicas int32) error {
	if replicas < 0 || replicas > maxReplicas {
		return fmt.Errorf("%d is out of range", replicas)
	}

	deploymentsClient := m.client.Deployments(m.namespace)

	scale, err := deploymentsClient.GetScale(context.TODO(), m.app.AppName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if scale.Spec.Replicas == replicas {
		return nil
	}

	scale.Spec.Replicas = replicas
	m.app.Replicas = replicas

	_, err = deploymentsClient.UpdateScale(context.TODO(), m.app.AppName, scale, metav1.UpdateOptions{})

	return err
}

// CreateIngressService creates Ingress endpoint for test app
func (m *AppManager) CreateIngressService() (*apiv1.Service, error) {
	serviceClient := m.client.Services(m.namespace)
	obj := buildServiceObject(m.namespace, m.app)
	result, err := serviceClient.Create(context.TODO(), obj, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// AcquireExternalURL gets external ingress endpoint from service when it is ready
func (m *AppManager) AcquireExternalURL() string {
	log.Printf("Waiting until service ingress is ready for %s...\n", m.app.AppName)
	svc, err := m.WaitUntilServiceState(m.IsServiceIngressReady)
	if err != nil {
		return ""
	}

	log.Printf("Service ingress for %s is ready...\n", m.app.AppName)
	return m.AcquireExternalURLFromService(svc)
}

// WaitUntilServiceState waits until isState returns true
func (m *AppManager) WaitUntilServiceState(isState func(*apiv1.Service, error) bool) (*apiv1.Service, error) {
	serviceClient := m.client.Services(m.namespace)
	var lastService *apiv1.Service

	waitErr := wait.PollImmediate(PollInterval, PollTimeout, func() (bool, error) {
		var err error
		lastService, err = serviceClient.Get(context.TODO(), m.app.AppName, metav1.GetOptions{})
		done := isState(lastService, err)
		if !done && err != nil {
			return true, err
		}

		return done, nil
	})

	if waitErr != nil {
		return lastService, fmt.Errorf("service %q is not in desired state, received: %+v: %s", m.app.AppName, lastService, waitErr)
	}

	return lastService, nil
}

// AcquireExternalURLFromService gets external url from Service Object.
func (m *AppManager) AcquireExternalURLFromService(svc *apiv1.Service) string {
	if svc.Status.LoadBalancer.Ingress != nil && len(svc.Status.LoadBalancer.Ingress) > 0 && len(svc.Spec.Ports) > 0 {
		address := ""
		if svc.Status.LoadBalancer.Ingress[0].Hostname != "" {
			address = svc.Status.LoadBalancer.Ingress[0].Hostname
		} else {
			address = svc.Status.LoadBalancer.Ingress[0].IP
		}
		return fmt.Sprintf("%s:%d", address, svc.Spec.Ports[0].Port)
	}

	// TODO: Support the other local k8s clusters
	if minikubeExternalIP := m.minikubeNodeIP(); minikubeExternalIP != "" {
		// if test cluster is minikube, external ip address is minikube node address
		if len(svc.Spec.Ports) > 0 {
			return fmt.Sprintf("%s:%d", minikubeExternalIP, svc.Spec.Ports[0].NodePort)
		}
	}

	return ""
}

// IsServiceIngressReady returns true if external ip is available
func (m *AppManager) IsServiceIngressReady(svc *apiv1.Service, err error) bool {
	if err != nil || svc == nil {
		return false
	}

	if svc.Status.LoadBalancer.Ingress != nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
		return true
	}

	// TODO: Support the other local k8s clusters
	if m.minikubeNodeIP() != "" {
		if len(svc.Spec.Ports) > 0 {
			return true
		}
	}

	return false
}

// IsServiceDeleted returns true if service does not exist
func (m *AppManager) IsServiceDeleted(svc *apiv1.Service, err error) bool {
	return err != nil && errors.IsNotFound(err)
}

func (m *AppManager) minikubeNodeIP() string {
	// if you are running the test in minikube environment, DAPR_TEST_MINIKUBE_IP environment variable must be
	// minikube cluster IP address from the output of `minikube ip` command

	// TODO: Use the better way to get the node ip of minikube
	return os.Getenv(MiniKubeIPEnvVar)
}

// DeleteDeployment deletes deployment for the test app
func (m *AppManager) DeleteDeployment(ignoreNotFound bool) error {
	deploymentsClient := m.client.Deployments(m.namespace)
	deletePolicy := metav1.DeletePropagationForeground

	if err := deploymentsClient.Delete(context.TODO(), m.app.AppName, metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}); err != nil && (ignoreNotFound && !errors.IsNotFound(err)) {
		return err
	}

	return nil
}

// DeleteService deletes deployment for the test app
func (m *AppManager) DeleteService(ignoreNotFound bool) error {
	serviceClient := m.client.Services(m.namespace)
	deletePolicy := metav1.DeletePropagationForeground

	if err := serviceClient.Delete(context.TODO(), m.app.AppName, metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}); err != nil && (ignoreNotFound && !errors.IsNotFound(err)) {
		return err
	}

	return nil
}

// GetOrCreateNamespace gets or creates namespace unless namespace exists
func (m *AppManager) GetOrCreateNamespace() (*apiv1.Namespace, error) {
	namespaceClient := m.client.Namespaces()
	ns, err := namespaceClient.Get(context.TODO(), m.namespace, metav1.GetOptions{})

	if err != nil && errors.IsNotFound(err) {
		obj := buildNamespaceObject(m.namespace)
		ns, err = namespaceClient.Create(context.TODO(), obj, metav1.CreateOptions{})
		return ns, err
	}

	return ns, err
}

// GetHostDetails returns the name and IP address of the pods running the app
func (m *AppManager) GetHostDetails() ([]PodInfo, error) {
	if !m.app.DaprEnabled {
		return nil, fmt.Errorf("dapr is not enabled for this app")
	}

	podClient := m.client.Pods(m.namespace)

	// Filter only 'testapp=appName' labeled Pods
	podList, err := podClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", TestAppLabelKey, m.app.AppName),
	})
	if err != nil {
		return nil, err
	}

	if len(podList.Items) != int(m.app.Replicas) {
		return nil, fmt.Errorf("expected number of pods for %s: %d, received: %d", m.app.AppName, m.app.Replicas, len(podList.Items))
	}

	result := make([]PodInfo, 0, len(podList.Items))
	for _, item := range podList.Items {
		result = append(result, PodInfo{
			Name: item.GetName(),
			IP:   item.Status.PodIP,
		})
	}

	return result, nil
}

// SaveContainerLogs get container logs for all containers in the pod and saves them to disk
func (m *AppManager) SaveContainerLogs() error {
	if !m.app.DaprEnabled {
		return fmt.Errorf("dapr is not enabled for this app")
	}

	podClient := m.client.Pods(m.namespace)

	// Filter only 'testapp=appName' labeled Pods
	podList, err := podClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", TestAppLabelKey, m.app.AppName),
	})
	if err != nil {
		return err
	}

	for _, pod := range podList.Items {
		for _, container := range pod.Spec.Containers {
			err := func() error {
				req := podClient.GetLogs(pod.GetName(), &apiv1.PodLogOptions{
					Container: container.Name,
				})
				podLogs, err := req.Stream(context.TODO())
				if err != nil {
					return err
				}
				defer podLogs.Close()

				filename := fmt.Sprintf("%s/%s.%s.log", m.logPrefix, pod.GetName(), container.Name)
				fh, err := os.Create(filename)
				if err != nil {
					return err
				}
				defer fh.Close()
				_, err = io.Copy(fh, podLogs)
				if err != nil {
					return err
				}

				log.Printf("Saved container logs to %s", filename)
				return nil
			}()

			if err != nil {
				return err
			}
		}
	}

	return nil
}

// GetCPUAndMemory returns the Cpu and Memory usage for the dapr app or sidecar
func (m *AppManager) GetCPUAndMemory(sidecar bool) (int64, float64, error) {
	pods, err := m.GetHostDetails()
	if err != nil {
		return -1, -1, err
	}

	var maxCPU int64 = -1
	var maxMemory float64 = -1
	for _, pod := range pods {
		podName := pod.Name
		metrics, err := m.client.MetricsClient.MetricsV1beta1().PodMetricses(m.namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return -1, -1, err
		}

		for _, c := range metrics.Containers {
			isSidecar := c.Name == DaprSideCarName
			if isSidecar == sidecar {
				mi, _ := c.Usage.Memory().AsInt64()
				mb := float64((mi / 1024)) * 0.001024

				cpu := c.Usage.Cpu().ScaledValue(resource.Milli)

				if cpu > maxCPU {
					maxCPU = cpu
				}

				if mb > maxMemory {
					maxMemory = mb
				}
			}
		}
	}
	if (maxCPU < 0) || (maxMemory < 0) {
		return -1, -1, fmt.Errorf("container (sidecar=%v) not found in pods for app %s in namespace %s", sidecar, m.app.AppName, m.namespace)
	}

	return maxCPU, maxMemory, nil
}

// GetTotalRestarts returns the total number of restarts for the app or sidecar
func (m *AppManager) GetTotalRestarts() (int, error) {
	if !m.app.DaprEnabled {
		return 0, fmt.Errorf("dapr is not enabled for this app")
	}

	podClient := m.client.Pods(m.namespace)

	// Filter only 'testapp=appName' labeled Pods
	podList, err := podClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", TestAppLabelKey, m.app.AppName),
	})
	if err != nil {
		return 0, err
	}

	restartCount := 0
	for _, pod := range podList.Items {
		pod, err := podClient.Get(context.TODO(), pod.GetName(), metav1.GetOptions{})
		if err != nil {
			return 0, err
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			restartCount += int(containerStatus.RestartCount)
		}
	}

	return restartCount, nil
}
