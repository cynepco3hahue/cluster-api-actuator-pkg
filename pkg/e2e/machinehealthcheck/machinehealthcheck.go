package machinehealthcheck

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/golang/glog"
	e2e "github.com/openshift/cluster-api-actuator-pkg/pkg/e2e/framework"
	mapiv1beta1 "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	"github.com/openshift/machine-api-operator/pkg/util/conditions"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	machineAPIControllers       = "machine-api-controllers"
	machineHealthCheckControler = "machine-healthcheck-controller"
)

var _ = Describe("[TechPreview:Feature:MachineHealthCheck] MachineHealthCheck controller", func() {
	var client runtimeclient.Client
	var numberOfReadyWorkers int
	var workerNode *corev1.Node
	var workerMachine *mapiv1beta1.Machine

	stopKubeletAndValidateMachineDeletion := func(workerNodeName *corev1.Node, workerMachine *mapiv1beta1.Machine, timeout time.Duration) {
		By(fmt.Sprintf("Stopping kubelet service on the node %s", workerNode.Name))
		err := e2e.StopKubelet(workerNode.Name)
		Expect(err).ToNot(HaveOccurred())

		By(fmt.Sprintf("Validating that node %s has 'NotReady' condition", workerNode.Name))
		waitForNodeUnhealthyCondition(workerNode.Name)

		By(fmt.Sprintf("Validating that machine %s is deleted", workerMachine.Name))
		machine := &mapiv1beta1.Machine{}
		key := types.NamespacedName{
			Namespace: workerMachine.Namespace,
			Name:      workerMachine.Name,
		}
		Eventually(func() bool {
			err := client.Get(context.TODO(), key, machine)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return true
				}
			}
			glog.V(2).Infof("machine deletion timestamp %s still exists", machine.DeletionTimestamp)
			return false
		}, timeout, 5*time.Second).Should(BeTrue())
	}

	BeforeEach(func() {
		var err error
		client, err = e2e.LoadClient()
		Expect(err).ToNot(HaveOccurred())

		err = e2e.CreateOrUpdateTechPreviewFeatureGate()
		Expect(err).ToNot(HaveOccurred())

		// Wait until the deployment with machine-healthcheck controller will be ready
		Eventually(func() bool {
			d, err := e2e.GetDeployment(client, machineAPIControllers)
			if err != nil {
				return false
			}
			return e2e.DeploymentHasContainer(d, machineHealthCheckControler)
		}, e2e.WaitLong, 10*time.Second).Should(BeTrue())

		Expect(e2e.IsDeploymentAvailable(client, machineAPIControllers)).Should(BeTrue())

		workerNodes, err := e2e.GetWorkerNodes(client)
		Expect(err).ToNot(HaveOccurred())

		readyWorkerNodes := e2e.FilterReadyNodes(workerNodes)
		Expect(readyWorkerNodes).ToNot(BeEmpty())

		numberOfReadyWorkers = len(readyWorkerNodes)
		workerNode = &readyWorkerNodes[0]
		glog.V(2).Infof("Worker node %s", workerNode.Name)

		workerMachine, err = e2e.GetMachineFromNode(client, workerNode)
		Expect(err).ToNot(HaveOccurred())
		glog.V(2).Infof("Worker machine %s", workerMachine.Name)

		glog.V(2).Infof("Create machine health check with label selector: %s", workerMachine.Labels)
		err = e2e.CreateMachineHealthCheck(workerMachine.Labels)
		Expect(err).ToNot(HaveOccurred())
	})

	Context("with node-unhealthy-conditions configmap", func() {
		BeforeEach(func() {
			unhealthyConditions := &conditions.UnhealthyConditions{
				Items: []conditions.UnhealthyCondition{
					{
						Name:    "Ready",
						Status:  "Unknown",
						Timeout: "60s",
					},
				},
			}
			glog.V(2).Infof("Create node-unhealthy-conditions configmap")
			err := e2e.CreateUnhealthyConditionsConfigMap(unhealthyConditions)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should delete unhealthy machine", func() {
			stopKubeletAndValidateMachineDeletion(workerNode, workerMachine, 2*time.Minute)
		})

		AfterEach(func() {
			glog.V(2).Infof("Delete node-unhealthy-conditions configmap")
			err := e2e.DeleteUnhealthyConditionsConfigMap()
			Expect(err).ToNot(HaveOccurred())
		})
	})

	It("should delete unhealthy machine", func() {
		stopKubeletAndValidateMachineDeletion(workerNode, workerMachine, 6*time.Minute)
	})

	AfterEach(func() {
		waitForWorkersToGetReady(numberOfReadyWorkers)
		e2e.DeleteMachineHealthCheck(e2e.MachineHealthCheckName)
		e2e.DeleteKubeletKillerPods()
	})
})

func waitForNodeUnhealthyCondition(workerNodeName string) {
	client, err := e2e.LoadClient()
	Expect(err).ToNot(HaveOccurred())

	key := types.NamespacedName{
		Name:      workerNodeName,
		Namespace: e2e.TestContext.MachineApiNamespace,
	}
	node := &corev1.Node{}
	glog.Infof("Wait until node %s will have 'Ready' condition with the status %s", node.Name, corev1.ConditionUnknown)
	Eventually(func() bool {
		err := client.Get(context.TODO(), key, node)
		if err != nil {
			return false
		}
		readyCond := conditions.GetNodeCondition(node, corev1.NodeReady)
		glog.V(2).Infof("Node %s has 'Ready' condition with the status %s", node.Name, readyCond.Status)
		return readyCond.Status == corev1.ConditionUnknown
	}, e2e.WaitLong, 10*time.Second).Should(BeTrue())
}

func waitForWorkersToGetReady(numberOfReadyWorkers int) {
	client, err := e2e.LoadClient()
	Expect(err).ToNot(HaveOccurred())

	glog.V(2).Infof("Wait until the environment will have %d ready workers", numberOfReadyWorkers)
	Eventually(func() bool {
		workerNodes, err := e2e.GetWorkerNodes(client)
		if err != nil {
			return false
		}

		readyWorkerNodes := e2e.FilterReadyNodes(workerNodes)
		glog.V(2).Infof("Number of ready workers %d", len(readyWorkerNodes))
		return len(readyWorkerNodes) == numberOfReadyWorkers
	}, 15*time.Minute, 10*time.Second).Should(BeTrue())
}
