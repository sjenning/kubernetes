/*
Copyright 2017 The Kubernetes Authors.

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

package cpuset

import (
	"fmt"
	"github.com/golang/glog"
	"strconv"
	"sync"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/api/v1"
	internalapi "k8s.io/kubernetes/pkg/kubelet/apis/cri"
	runtimeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

// Manager is responsible for managing the cpusets of containers on the node
type Manager interface {
	Start()

	// RegisterContainer registers a container with the cpuset manager
	// resulting in synchronous setting on the container cpuset.  This
	// is called after RegisterContainer(), which creates the containerID, and
	// before StartContainer(), such that the cpuset is configured before
	// the container starts
	RegisterContainer(p *v1.Pod, c *v1.Container, containerID string) error

	// UnregisterContainer is called near UnregisterContainer() so that the manager
	// stops trying to update that container in the reconcilation loop and
	// any CPUs dedicated to the container are freed to the shared pool.
	UnregisterContainer(containerID string) error
}

type noopManager struct{}

var _ Manager = &noopManager{}

// NewNoopManager returns a cupset manager that does nothing
func NewNoopManager() (Manager, error) {
	return &noopManager{}, nil
}

func (m *noopManager) Start() {
}

func (m *noopManager) RegisterContainer(p *v1.Pod, c *v1.Container, containerID string) error {
	glog.Errorf("SETH noopManager RegisterContainer")
	return nil
}

func (m *noopManager) UnregisterContainer(containerID string) error {
	glog.Errorf("SETH noopManager UnregisterContainer")
	return nil
}

type cpuInfo struct {
	nodeId    int64
	coreId    int64
	assigment string
}

type staticManager struct {
	sync.Mutex

	// containerRuntime is the container runtime service interface needed
	// to make UpdateContainerResources() calls against the containers
	containerRuntime internalapi.RuntimeService

	// kletGetter provides a methods for obtaining various data from kubelet
	kletGetter kletGetter

	// podStatusProvider provides a method for obtaining pod statuses
	// and the containerID of their containers
	podStatusProvider status.PodStatusProvider

	//allCpus
	allCpus int64

	// sharedContainers is a map of containerIDs to their cached cpusets.
	// This is to avoid unnecessary UpdateContainerResources() calls.
	sharedContainers map[string]string

	// cached string version of the shared cpuset derived from cpuAssignments
	sharedCpuset string

	// cpuAssignmentState is an array for determining if a particular cpu is
	// assigned to a container or not
	cpuAssignmentState []cpuInfo

	// numSharedCpus is the number of cpus in the shared pool i.e. cpus
	// available for allocation to guaranteed containers.
	numSharedCpus int64
}

var _ Manager = &staticManager{}

type kletGetter interface {
	GetPods() []*v1.Pod
	GetCachedMachineInfo() (*cadvisorapi.MachineInfo, error)
}

func guaranteedCpus(pod *v1.Pod, container *v1.Container) int64 {
	if qos.GetPodQOS(pod) != v1.PodQOSGuaranteed {
		return 0
	}
	cpuQuantity := container.Resources.Requests[v1.ResourceCPU]
	glog.Errorf("SETH guaranteedCpus container: %s, cpu request: %v", container.Name, cpuQuantity.MilliValue())
	if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
		return 0
	}
	return cpuQuantity.Value()
}

func findContainerIDByName(status *v1.PodStatus, name string) (string, error) {
	for _, container := range status.ContainerStatuses {
		if container.Name == name && container.ContainerID != "" {
			// hack hack strip docker:// hack hack
			return container.ContainerID[9:], nil
		}
	}
	return "", fmt.Errorf("SETH unable to find container with name %v in pod status", name)
}

func (m *staticManager) updateSharedCpuset() {
	cpuset := ""
	for cpu, info := range m.cpuAssignmentState {
		if info.assigment != "" {
			continue
		}
		if cpuset != "" {
			cpuset += ","
		}
		cpuset += strconv.Itoa(cpu)
	}
	glog.Errorf("SETH updateSharedCpuset %v", cpuset)
	m.sharedCpuset = cpuset
}

func (m *staticManager) reconcileSharedContainers() {
	glog.Errorf("SETH staticManager reconcileSharedPoolContainers")
	m.Lock()
	defer m.Unlock()
	pods := m.kletGetter.GetPods()
	//glog.Errorf("SETH pods %v", pods)
	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			glog.Errorf("SETH pod: %v, container %v", pod.Name, container.Name)
			status, _ := m.podStatusProvider.GetPodStatus(pod.UID)
			containerID, err := findContainerIDByName(&status, container.Name)
			if err != nil {
				glog.Errorf("SETH failed to update cpuset for container %v: %v", container.Name, err)
				continue
			}
			glog.Errorf("SETH containerID %v", containerID)
			if guaranteedCpus(pod, &container) != 0 {
				continue
			}
			cpuset, ok := m.sharedContainers[containerID]
			if ok {
				glog.Errorf("SETH staticManager %v cpuset is %v", containerID, cpuset)
			} else {
				glog.Errorf("SETH staticManager encountered new %v cpuset is %v", containerID, cpuset)
			}
			if ok && cpuset == m.sharedCpuset {
				continue
			}
			glog.Errorf("SETH staticManager updating %v cpuset to %v", containerID, m.sharedCpuset)
			err = m.containerRuntime.UpdateContainerResources(
				containerID,
				&runtimeapi.LinuxContainerResources{
					CpusetCpus: m.sharedCpuset,
				})
			if err != nil {
				glog.Errorf("SETH failed updating %v cpuset to %v", containerID, m.sharedCpuset)
				continue
			}
			m.sharedContainers[containerID] = m.sharedCpuset
		}
	}
}

func (m *staticManager) assignCpu(containerID string, cpu int) {
	glog.Errorf("SETH assignCpu %v %v", containerID, cpu)
	m.cpuAssignmentState[cpu].assigment = containerID
	m.numSharedCpus--
	m.updateSharedCpuset()
}

func (m *staticManager) releaseCpu(cpu int) {
	glog.Errorf("SETH releaseCpu %v belonging to %v", cpu, m.cpuAssignmentState[cpu])
	m.cpuAssignmentState[cpu].assigment = ""
	m.numSharedCpus++
	m.updateSharedCpuset()
}

func (m *staticManager) allocateCpus(containerID string, numCpus int64) (string, error) {
	glog.Errorf("SETH allocateCpus %v %v", containerID, numCpus)
	if numCpus > m.numSharedCpus {
		return "", fmt.Errorf("not enough cpus available to satisfy request")
	}
	cpuset := ""
	cpusLeft := numCpus
	for cpu, info := range m.cpuAssignmentState {
		if info.assigment != "" {
			continue
		}
		m.assignCpu(containerID, cpu)
		if cpuset != "" {
			cpuset += ","
		}
		cpuset += strconv.Itoa(cpu)
		cpusLeft--
		if cpusLeft == 0 {
			glog.Errorf("SETH allocateCpus returning %v", cpuset)
			return cpuset, nil
		}
	}
	// should never get here, TODO add rollback to recover
	return "", fmt.Errorf("not enough cpus available to satisfy request")
}

// NewStaticManager returns a cupset manager that statically assigns cpus
func NewStaticManager(kletGetter kletGetter, podStatusProvider status.PodStatusProvider, containerRuntime internalapi.RuntimeService) (Manager, error) {
	glog.Errorf("SETH NewStaticManager")
	machinInfo, err := kletGetter.GetCachedMachineInfo()
	if err != nil {
		return nil, err
	}

	if machinInfo.NumCores == 0 {
		return nil, fmt.Errorf("could not detect number of cpus")
	}

	allCpus := int64(machinInfo.NumCores)
	glog.V(3).Infof("SETH allCpus: %v", allCpus)

	cpuAssignmentState := make([]cpuInfo, allCpus)
	for _, node := range machinInfo.Topology {
		for _, core := range node.Cores {
			for _, thread := range core.Threads {
				if int64(thread) >= allCpus {
					return nil, fmt.Errorf("thread id greather than number of cpus")
				}
				cpuAssignmentState[thread] = cpuInfo{
					nodeId:    int64(node.Id),
					coreId:    int64(core.Id),
					assigment: "",
				}
			}
		}
	}

	m := &staticManager{
		containerRuntime:   containerRuntime,
		kletGetter:         kletGetter,
		podStatusProvider:  podStatusProvider,
		allCpus:            allCpus,
		cpuAssignmentState: cpuAssignmentState,
		numSharedCpus:      allCpus,
		sharedContainers:   map[string]string{},
	}

	// reserve cpu 0 for system processes
	// TODO: this relates to system-reserved and needs to match up
	m.assignCpu("reserved", 0)

	return m, nil
}

func (m *staticManager) Start() {
	go wait.Until(m.reconcileSharedContainers, time.Second, wait.NeverStop)
}

// RegisterContainer registers a container with the cpu manager.  This should be
// called between CreateContainer and StartContainer
func (m *staticManager) RegisterContainer(pod *v1.Pod, container *v1.Container, containerID string) error {
	glog.Errorf("SETH staticManager RegisterContainer")
	m.Lock()
	defer m.Unlock()
	if numCpus := guaranteedCpus(pod, container); numCpus != 0 {
		// guaranteed container
		cpuset, err := m.allocateCpus(containerID, numCpus)
		if err != nil {
			glog.Errorf("SETH staticManager RegisterContainer error: %v", err)
			return err
		}
		glog.Errorf("SETH staticManager RegisterContainer guaranteed cpuset: %v", cpuset)
		err = m.containerRuntime.UpdateContainerResources(
			containerID,
			&runtimeapi.LinuxContainerResources{
				CpusetCpus: cpuset,
			})
		if err != nil {
			glog.Errorf("SETH staticManager RegisterContainer guaranteed error: %v", err)
			return err
		}
		return nil
	}

	// shared container
	glog.Errorf("SETH staticManager RegisterContainer shared cpuset: %v", m.sharedCpuset)
	err := m.containerRuntime.UpdateContainerResources(
		containerID,
		&runtimeapi.LinuxContainerResources{
			CpusetCpus: m.sharedCpuset,
		})
	if err != nil {
		glog.Errorf("SETH staticManager RegisterContainer shared error: %v", err)
		return err
	}
	m.sharedContainers[containerID] = m.sharedCpuset

	return nil
}

// UnregisterContainer unregisters a container from the cpu manager.  This
// should be called after the container processes have terminated.
func (m *staticManager) UnregisterContainer(containerID string) error {
	glog.Errorf("SETH staticManager UnregisterContainer")
	m.Lock()
	defer m.Unlock()
	for cpu, info := range m.cpuAssignmentState {
		if info.assigment == containerID {
			m.releaseCpu(cpu)
		}
	}
	return nil
}
