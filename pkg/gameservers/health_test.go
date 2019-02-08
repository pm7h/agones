// Copyright 2018 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gameservers

import (
	"testing"
	"time"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	agtesting "agones.dev/agones/pkg/testing"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
)

func TestHealthControllerFailedContainer(t *testing.T) {
	t.Parallel()

	m := agtesting.NewMocks()
	hc := NewHealthController(m.KubeClient, m.AgonesClient, m.KubeInformerFactory, m.AgonesInformerFactory)

	gs := v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test"}, Spec: newSingleContainerSpec()}
	gs.ApplyDefaults()

	pod, err := gs.Pod()
	assert.Nil(t, err)
	pod.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: gs.Spec.Container,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}}

	assert.True(t, hc.failedContainer(pod))
	pod2 := pod.DeepCopy()

	pod.Status.ContainerStatuses[0].State.Terminated = nil
	assert.False(t, hc.failedContainer(pod))

	pod2.Status.ContainerStatuses[0].Name = "Not a matching name"
	assert.False(t, hc.failedContainer(pod2))
}

func TestHealthUnschedulableWithNoFreePorts(t *testing.T) {
	t.Parallel()

	m := agtesting.NewMocks()
	hc := NewHealthController(m.KubeClient, m.AgonesClient, m.KubeInformerFactory, m.AgonesInformerFactory)

	gs := v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test"}, Spec: newSingleContainerSpec()}
	gs.ApplyDefaults()

	pod, err := gs.Pod()
	assert.Nil(t, err)

	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Reason: corev1.PodReasonUnschedulable,
			Message: "0/4 nodes are available: 4 node(s) didn't have free ports for the requestedpod ports."},
	}
	assert.True(t, hc.unschedulableWithNoFreePorts(pod))

	pod.Status.Conditions[0].Message = "not a real reason"
	assert.False(t, hc.unschedulableWithNoFreePorts(pod))
}

func TestHealthControllerSyncGameServer(t *testing.T) {
	t.Parallel()

	type expected struct {
		updated bool
		state   v1alpha1.GameServerState
	}
	fixtures := map[string]struct {
		state    v1alpha1.GameServerState
		expected expected
	}{
		"started": {
			state: v1alpha1.GameServerStateStarting,
			expected: expected{
				updated: true,
				state:   v1alpha1.GameServerStateUnhealthy,
			},
		},
		"scheduled": {
			state: v1alpha1.GameServerStateScheduled,
			expected: expected{
				updated: false,
				state:   v1alpha1.GameServerStateScheduled,
			},
		},
		"ready": {
			state: v1alpha1.GameServerStateReady,
			expected: expected{
				updated: true,
				state:   v1alpha1.GameServerStateUnhealthy,
			},
		},
	}

	for name, test := range fixtures {
		t.Run(name, func(t *testing.T) {
			m := agtesting.NewMocks()
			hc := NewHealthController(m.KubeClient, m.AgonesClient, m.KubeInformerFactory, m.AgonesInformerFactory)
			hc.recorder = m.FakeRecorder

			gs := v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test"}, Spec: newSingleContainerSpec(),
				Status: v1alpha1.GameServerStatus{State: test.state}}
			gs.ApplyDefaults()

			got := false
			updated := false
			m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
				got = true
				return true, &v1alpha1.GameServerList{Items: []v1alpha1.GameServer{gs}}, nil
			})
			m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
				updated = true
				ua := action.(k8stesting.UpdateAction)
				gsObj := ua.GetObject().(*v1alpha1.GameServer)
				assert.Equal(t, test.expected.state, gsObj.Status.State)
				return true, gsObj, nil
			})

			_, cancel := agtesting.StartInformers(m)
			defer cancel()

			err := hc.syncGameServer("default/test")
			assert.Nil(t, err, err)
			assert.True(t, got, "GameServers Should be got!")

			assert.Equal(t, test.expected.updated, updated, "updated test")
		})
	}
}

func TestHealthControllerRun(t *testing.T) {
	m := agtesting.NewMocks()
	hc := NewHealthController(m.KubeClient, m.AgonesClient, m.KubeInformerFactory, m.AgonesInformerFactory)
	hc.recorder = m.FakeRecorder

	gsWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))

	podWatch := watch.NewFake()
	m.KubeClient.AddWatchReactor("pods", k8stesting.DefaultWatchReactor(podWatch, nil))

	updated := make(chan bool)
	defer close(updated)
	m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		defer func() {
			updated <- true
		}()
		ua := action.(k8stesting.UpdateAction)
		gsObj := ua.GetObject().(*v1alpha1.GameServer)
		assert.Equal(t, v1alpha1.GameServerStateUnhealthy, gsObj.Status.State)
		return true, gsObj, nil
	})

	gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test"}, Spec: newSingleContainerSpec(),
		Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}}
	gs.ApplyDefaults()
	pod, err := gs.Pod()
	assert.Nil(t, err)

	stop, cancel := agtesting.StartInformers(m)
	defer cancel()

	go hc.Run(stop)

	gsWatch.Add(gs)
	podWatch.Add(pod)

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: gs.Spec.Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	// gate
	assert.True(t, hc.failedContainer(pod))
	assert.False(t, hc.unschedulableWithNoFreePorts(pod))

	podWatch.Modify(pod)

	select {
	case <-updated:
	case <-time.After(10 * time.Second):
		assert.FailNow(t, "timeout on GameServer update")
	}

	agtesting.AssertEventContains(t, m.FakeRecorder.Events, string(v1alpha1.GameServerStateUnhealthy))

	pod.Status.ContainerStatuses = nil
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Reason: corev1.PodReasonUnschedulable,
			Message: "0/4 nodes are available: 4 node(s) didn't have free ports for the requestedpod ports."},
	}
	// gate
	assert.True(t, hc.unschedulableWithNoFreePorts(pod))
	assert.False(t, hc.failedContainer(pod))

	podWatch.Modify(pod)

	select {
	case <-updated:
	case <-time.After(10 * time.Second):
		assert.FailNow(t, "timeout on GameServer update")
	}

	agtesting.AssertEventContains(t, m.FakeRecorder.Events, string(v1alpha1.GameServerStateUnhealthy))
}
