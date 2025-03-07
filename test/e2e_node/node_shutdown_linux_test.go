//go:build linux
// +build linux

/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2enode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/test/e2e/framework"

	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
)

var _ = SIGDescribe("GracefulNodeShutdown [Serial] [NodeFeature:GracefulNodeShutdown] [NodeFeature:GracefulNodeShutdownBasedOnPodPriority]", func() {
	f := framework.NewDefaultFramework("graceful-node-shutdown")
	ginkgo.Context("when gracefully shutting down", func() {

		const (
			pollInterval                        = 1 * time.Second
			podStatusUpdateTimeout              = 30 * time.Second
			nodeStatusUpdateTimeout             = 30 * time.Second
			nodeShutdownGracePeriod             = 20 * time.Second
			nodeShutdownGracePeriodCriticalPods = 10 * time.Second
		)

		tempSetCurrentKubeletConfig(f, func(initialConfig *kubeletconfig.KubeletConfiguration) {
			initialConfig.FeatureGates = map[string]bool{
				string(features.GracefulNodeShutdown):                   true,
				string(features.GracefulNodeShutdownBasedOnPodPriority): false,
			}
			initialConfig.ShutdownGracePeriod = metav1.Duration{Duration: nodeShutdownGracePeriod}
			initialConfig.ShutdownGracePeriodCriticalPods = metav1.Duration{Duration: nodeShutdownGracePeriodCriticalPods}
		})

		ginkgo.BeforeEach(func() {
			if err := lookEmitSignalCommand(); err != nil {
				e2eskipper.Skipf("skipping test because: %v", err)
				return
			}
			ginkgo.By("Wait for the node to be ready")
			waitForNodeReady()
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Emitting Shutdown false signal; cancelling the shutdown")
			err := emitSignalPrepareForShutdown(false)
			framework.ExpectNoError(err)
		})

		ginkgo.It("should be able to gracefully shutdown pods with various grace periods", func() {
			nodeName := getNodeName(f)
			nodeSelector := fields.Set{
				"spec.nodeName": nodeName,
			}.AsSelector().String()

			// Define test pods
			pods := []*v1.Pod{
				getGracePeriodOverrideTestPod("period-120", nodeName, 120, ""),
				getGracePeriodOverrideTestPod("period-5", nodeName, 5, ""),
				getGracePeriodOverrideTestPod("period-critical-120", nodeName, 120, scheduling.SystemNodeCritical),
				getGracePeriodOverrideTestPod("period-critical-5", nodeName, 5, scheduling.SystemNodeCritical),
			}

			ginkgo.By("Creating batch pods")
			f.PodClient().CreateBatch(pods)

			list, err := f.PodClient().List(context.TODO(), metav1.ListOptions{
				FieldSelector: nodeSelector,
			})
			framework.ExpectNoError(err)
			framework.ExpectEqual(len(list.Items), len(pods), "the number of pods is not as expected")

			ginkgo.By("Verifying batch pods are running")
			for _, pod := range list.Items {
				if podReady, err := testutils.PodRunningReady(&pod); err != nil || !podReady {
					framework.Failf("Failed to start batch pod: %v", pod.Name)
				}
			}

			ginkgo.By("Emitting shutdown signal")
			err = emitSignalPrepareForShutdown(true)
			framework.ExpectNoError(err)

			ginkgo.By("Verifying that non-critical pods are shutdown")
			// Not critical pod should be shutdown
			gomega.Eventually(func() error {
				list, err = f.PodClient().List(context.TODO(), metav1.ListOptions{
					FieldSelector: nodeSelector,
				})
				if err != nil {
					return err
				}
				framework.ExpectEqual(len(list.Items), len(pods), "the number of pods is not as expected")

				for _, pod := range list.Items {
					if isPodStatusAffectedByIssue108594(&pod) {
						framework.Logf("Detected invalid pod state for pod %q: pod status: %+v", pod.Name, pod.Status)
						framework.Failf("failing test due to detecting invalid pod status")
					}

					if kubelettypes.IsCriticalPod(&pod) {
						if isPodShutdown(&pod) {
							framework.Logf("Expecting critical pod to be running, but it's not currently. Pod: %q, Pod Status %+v", pod.Name, pod.Status)
							return fmt.Errorf("critical pod should not be shutdown, phase: %s", pod.Status.Phase)
						}
					} else {
						if !isPodShutdown(&pod) {
							framework.Logf("Expecting non-critical pod to be shutdown, but it's not currently. Pod: %q, Pod Status %+v", pod.Name, pod.Status)
							return fmt.Errorf("pod should be shutdown, phase: %s", pod.Status.Phase)
						}
					}
				}
				return nil
			}, podStatusUpdateTimeout, pollInterval).Should(gomega.BeNil())

			ginkgo.By("Verifying that all pods are shutdown")
			// All pod should be shutdown
			gomega.Eventually(func() error {
				list, err = f.PodClient().List(context.TODO(), metav1.ListOptions{
					FieldSelector: nodeSelector,
				})
				if err != nil {
					return err
				}
				framework.ExpectEqual(len(list.Items), len(pods), "the number of pods is not as expected")

				for _, pod := range list.Items {
					if isPodStatusAffectedByIssue108594(&pod) {
						framework.Logf("Detected invalid pod state for pod %q: pod status: %+v", pod.Name, pod.Status)
						framework.Failf("failing test due to detecting invalid pod status")
					}
					if !isPodShutdown(&pod) {
						framework.Logf("Expecting pod to be shutdown, but it's not currently: Pod: %q, Pod Status %+v", pod.Name, pod.Status)
						return fmt.Errorf("pod should be shutdown, phase: %s", pod.Status.Phase)
					}
				}
				return nil
			},
				// Critical pod starts shutdown after (nodeShutdownGracePeriod-nodeShutdownGracePeriodCriticalPods)
				podStatusUpdateTimeout+(nodeShutdownGracePeriod-nodeShutdownGracePeriodCriticalPods),
				pollInterval).Should(gomega.BeNil())
		})

		ginkgo.It("should be able to handle a cancelled shutdown", func() {
			ginkgo.By("Emitting Shutdown signal")
			err := emitSignalPrepareForShutdown(true)
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				isReady := getNodeReadyStatus(f)
				if isReady {
					return fmt.Errorf("node did not become shutdown as expected")
				}
				return nil
			}, nodeStatusUpdateTimeout, pollInterval).Should(gomega.BeNil())

			ginkgo.By("Emitting Shutdown false signal; cancelling the shutdown")
			err = emitSignalPrepareForShutdown(false)
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				isReady := getNodeReadyStatus(f)
				if !isReady {
					return fmt.Errorf("node did not recover as expected")
				}
				return nil
			}, nodeStatusUpdateTimeout, pollInterval).Should(gomega.BeNil())
		})

		ginkgo.It("after restart dbus, should be able to gracefully shutdown", func() {
			// allows manual restart of dbus to work in Ubuntu.
			err := overlayDbusConfig()
			framework.ExpectNoError(err)
			defer func() {
				err := restoreDbusConfig()
				framework.ExpectNoError(err)
			}()

			ginkgo.By("Restart Dbus")
			err = restartDbus()
			framework.ExpectNoError(err)

			// Wait a few seconds to ensure dbus is restarted...
			time.Sleep(5 * time.Second)

			ginkgo.By("Emitting Shutdown signal")
			err = emitSignalPrepareForShutdown(true)
			framework.ExpectNoError(err)

			gomega.Eventually(func() error {
				isReady := getNodeReadyStatus(f)
				if isReady {
					return fmt.Errorf("node did not become shutdown as expected")
				}
				return nil
			}, nodeStatusUpdateTimeout, pollInterval).Should(gomega.BeNil())
		})
	})

	ginkgo.Context("when gracefully shutting down with Pod priority", func() {

		const (
			pollInterval                 = 1 * time.Second
			podStatusUpdateTimeout       = 10 * time.Second
			priorityClassesCreateTimeout = 10 * time.Second
		)

		var (
			customClassA = getPriorityClass("custom-class-a", 100000)
			customClassB = getPriorityClass("custom-class-b", 10000)
			customClassC = getPriorityClass("custom-class-c", 1000)
		)

		tempSetCurrentKubeletConfig(f, func(initialConfig *kubeletconfig.KubeletConfiguration) {
			initialConfig.FeatureGates = map[string]bool{
				string(features.GracefulNodeShutdown):                   true,
				string(features.GracefulNodeShutdownBasedOnPodPriority): true,
			}
			initialConfig.ShutdownGracePeriodByPodPriority = []kubeletconfig.ShutdownGracePeriodByPodPriority{
				{
					Priority:                   scheduling.SystemCriticalPriority,
					ShutdownGracePeriodSeconds: int64(podStatusUpdateTimeout / time.Second),
				},
				{
					Priority:                   customClassA.Value,
					ShutdownGracePeriodSeconds: int64(podStatusUpdateTimeout / time.Second),
				},
				{
					Priority:                   customClassB.Value,
					ShutdownGracePeriodSeconds: int64(podStatusUpdateTimeout / time.Second),
				},
				{
					Priority:                   customClassC.Value,
					ShutdownGracePeriodSeconds: int64(podStatusUpdateTimeout / time.Second),
				},
				{
					Priority:                   scheduling.DefaultPriorityWhenNoDefaultClassExists,
					ShutdownGracePeriodSeconds: int64(podStatusUpdateTimeout / time.Second),
				},
			}

		})

		ginkgo.BeforeEach(func() {
			if err := lookEmitSignalCommand(); err != nil {
				e2eskipper.Skipf("skipping test because: %v", err)
				return
			}

			ginkgo.By("Wait for the node to be ready")
			waitForNodeReady()
			customClasses := []*schedulingv1.PriorityClass{customClassA, customClassB, customClassC}
			for _, customClass := range customClasses {
				_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(context.Background(), customClass, metav1.CreateOptions{})
				if err != nil && !apierrors.IsAlreadyExists(err) {
					framework.ExpectNoError(err)
				}
			}
			gomega.Eventually(func() error {
				for _, customClass := range customClasses {
					_, err := f.ClientSet.SchedulingV1().PriorityClasses().Get(context.Background(), customClass.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
				}
				return nil
			}, priorityClassesCreateTimeout, pollInterval).Should(gomega.BeNil())
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Emitting Shutdown false signal; cancelling the shutdown")
			err := emitSignalPrepareForShutdown(false)
			framework.ExpectNoError(err)
		})

		ginkgo.It("should be able to gracefully shutdown pods with various grace periods", func() {
			nodeName := getNodeName(f)
			nodeSelector := fields.Set{
				"spec.nodeName": nodeName,
			}.AsSelector().String()

			// Define test pods
			pods := []*v1.Pod{
				getGracePeriodOverrideTestPod("period-5", nodeName, 5, ""),
				getGracePeriodOverrideTestPod("period-c-5", nodeName, 5, customClassC.Name),
				getGracePeriodOverrideTestPod("period-b-5", nodeName, 5, customClassB.Name),
				getGracePeriodOverrideTestPod("period-a-5", nodeName, 5, customClassA.Name),
				getGracePeriodOverrideTestPod("period-critical-5", nodeName, 5, scheduling.SystemNodeCritical),
			}

			// Expected down steps
			downSteps := [][]string{
				{
					"period-5",
				},
				{
					"period-5",
					"period-c-5",
				},
				{
					"period-5",
					"period-c-5",
					"period-b-5",
				},
				{
					"period-5",
					"period-c-5",
					"period-b-5",
					"period-a-5",
				},
				{
					"period-5",
					"period-c-5",
					"period-b-5",
					"period-a-5",
					"period-critical-5",
				},
			}

			ginkgo.By("Creating batch pods")
			f.PodClient().CreateBatch(pods)

			list, err := f.PodClient().List(context.TODO(), metav1.ListOptions{
				FieldSelector: nodeSelector,
			})
			framework.ExpectNoError(err)
			framework.ExpectEqual(len(list.Items), len(pods), "the number of pods is not as expected")

			ginkgo.By("Verifying batch pods are running")
			for _, pod := range list.Items {
				if podReady, err := testutils.PodRunningReady(&pod); err != nil || !podReady {
					framework.Failf("Failed to start batch pod: %v", pod.Name)
				}
			}

			ginkgo.By("Emitting shutdown signal")
			err = emitSignalPrepareForShutdown(true)
			framework.ExpectNoError(err)

			ginkgo.By("Verifying that pods are shutdown")

			for _, step := range downSteps {
				gomega.Eventually(func() error {
					list, err = f.PodClient().List(context.TODO(), metav1.ListOptions{
						FieldSelector: nodeSelector,
					})
					if err != nil {
						return err
					}
					framework.ExpectEqual(len(list.Items), len(pods), "the number of pods is not as expected")
					for _, pod := range list.Items {
						shouldShutdown := false
						for _, podName := range step {
							if podName == pod.Name {
								shouldShutdown = true
								break
							}
						}
						if !shouldShutdown {
							if pod.Status.Phase != v1.PodRunning {
								framework.Logf("Expecting pod to be running, but it's not currently. Pod: %q, Pod Status Phase: %q, Pod Status Reason: %q", pod.Name, pod.Status.Phase, pod.Status.Reason)
								return fmt.Errorf("pod should not be shutdown, phase: %s, reason: %s", pod.Status.Phase, pod.Status.Reason)
							}
						} else {
							if pod.Status.Reason != podShutdownReason {
								framework.Logf("Expecting pod to be shutdown, but it's not currently. Pod: %q, Pod Status Phase: %q, Pod Status Reason: %q", pod.Name, pod.Status.Phase, pod.Status.Reason)
								for _, item := range list.Items {
									framework.Logf("DEBUG %s, %s, %s", item.Name, item.Status.Phase, pod.Status.Reason)
								}
								return fmt.Errorf("pod should be shutdown, reason: %s", pod.Status.Reason)
							}
						}
					}
					return nil
				}, podStatusUpdateTimeout, pollInterval).Should(gomega.BeNil())
			}
		})
	})
})

func getPriorityClass(name string, value int32) *schedulingv1.PriorityClass {
	priority := &schedulingv1.PriorityClass{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PriorityClass",
			APIVersion: "scheduling.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Value: value,
	}
	return priority
}
func getGracePeriodOverrideTestPod(name string, node string, gracePeriod int64, priorityClassName string) *v1.Pod {
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    name,
					Image:   busyboxImage,
					Command: []string{"sh", "-c"},
					Args: []string{`
_term() {
	echo "Caught SIGTERM signal!"
	while true; do sleep 5; done
}
trap _term SIGTERM
while true; do sleep 5; done
`},
				},
			},
			TerminationGracePeriodSeconds: &gracePeriod,
			NodeName:                      node,
		},
	}
	if priorityClassName == scheduling.SystemNodeCritical {
		pod.ObjectMeta.Annotations = map[string]string{
			kubelettypes.ConfigSourceAnnotationKey: kubelettypes.FileSource,
		}
		pod.Spec.PriorityClassName = priorityClassName
		framework.ExpectEqual(kubelettypes.IsCriticalPod(pod), true, "pod should be a critical pod")
	} else {
		pod.Spec.PriorityClassName = priorityClassName
		framework.ExpectEqual(kubelettypes.IsCriticalPod(pod), false, "pod should not be a critical pod")
	}
	return pod
}

// Emits a fake PrepareForShutdown dbus message on system dbus. Will cause kubelet to react to an active shutdown event.
func emitSignalPrepareForShutdown(b bool) error {
	cmd := "dbus-send --system /org/freedesktop/login1 org.freedesktop.login1.Manager.PrepareForShutdown boolean:" + strconv.FormatBool(b)
	_, err := runCommand("sh", "-c", cmd)
	return err
}

func lookEmitSignalCommand() error {
	_, err := exec.LookPath("dbus-send")
	return err
}

func getNodeReadyStatus(f *framework.Framework) bool {
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	framework.ExpectNoError(err)
	// Assuming that there is only one node, because this is a node e2e test.
	framework.ExpectEqual(len(nodeList.Items), 1)
	return isNodeReady(&nodeList.Items[0])
}

func restartDbus() error {
	cmd := "systemctl restart dbus"
	_, err := runCommand("sh", "-c", cmd)
	return err
}

func systemctlDaemonReload() error {
	cmd := "systemctl daemon-reload"
	_, err := runCommand("sh", "-c", cmd)
	return err
}

var (
	dbusConfPath = "/etc/systemd/system/dbus.service.d/k8s-graceful-node-shutdown-e2e.conf"
	dbusConf     = `
[Unit]
RefuseManualStart=no
RefuseManualStop=no
[Service]
KillMode=control-group
ExecStop=
`
)

func overlayDbusConfig() error {
	err := os.MkdirAll(filepath.Dir(dbusConfPath), 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(dbusConfPath, []byte(dbusConf), 0644)
	if err != nil {
		return err
	}
	return systemctlDaemonReload()
}

func restoreDbusConfig() error {
	err := os.Remove(dbusConfPath)
	if err != nil {
		return err
	}
	return systemctlDaemonReload()
}

const (
	// https://github.com/kubernetes/kubernetes/blob/1dd781ddcad454cc381806fbc6bd5eba8fa368d7/pkg/kubelet/nodeshutdown/nodeshutdown_manager_linux.go#L43-L44
	podShutdownReason  = "Terminated"
	podShutdownMessage = "Pod was terminated in response to imminent node shutdown."
)

func isPodShutdown(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}

	hasContainersNotReadyCondition := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == v1.ContainersReady && cond.Status == v1.ConditionFalse {
			hasContainersNotReadyCondition = true
		}
	}

	return pod.Status.Message == podShutdownMessage && pod.Status.Reason == podShutdownReason && hasContainersNotReadyCondition && pod.Status.Phase == v1.PodFailed
}

// Pods should never report failed phase and have ready condition = true (https://github.com/kubernetes/kubernetes/issues/108594)
func isPodStatusAffectedByIssue108594(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed && podutils.IsPodReady(pod)
}
