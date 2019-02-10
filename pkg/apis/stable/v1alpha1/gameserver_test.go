// Copyright 2017 Google Inc. All Rights Reserved.
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

package v1alpha1

import (
	"fmt"
	"testing"

	"agones.dev/agones/pkg/apis/stable"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ipFixture = "127.1.1.1"
)

func TestGameServerFindGameServerContainer(t *testing.T) {
	t.Parallel()

	fixture := corev1.Container{Name: "mycontainer", Image: "foo/mycontainer"}
	gs := &GameServer{
		Spec: GameServerSpec{
			Container: "mycontainer",
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						fixture,
						{Name: "notmycontainer", Image: "foo/notmycontainer"},
					},
				},
			},
		},
	}

	i, container, err := gs.FindGameServerContainer()
	assert.Nil(t, err)
	assert.Equal(t, fixture, container)
	container.Ports = append(container.Ports, corev1.ContainerPort{HostPort: 1234})
	gs.Spec.Template.Spec.Containers[i] = container
	assert.Equal(t, gs.Spec.Template.Spec.Containers[0], container)
}

func TestGameServerApplyDefaults(t *testing.T) {
	t.Parallel()

	type expected struct {
		protocol   corev1.Protocol
		state      GameServerState
		policy     PortPolicy
		health     Health
		scheduling SchedulingStrategy
	}
	data := map[string]struct {
		gameServer GameServer
		container  string
		expected   expected
	}{
		"set basic defaults on a very simple gameserver": {
			gameServer: GameServer{
				Spec: GameServerSpec{
					Ports: []GameServerPort{{ContainerPort: 999}},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{
							{Name: "testing", Image: "testing/image"},
						}}}},
			},
			container: "testing",
			expected: expected{
				protocol:   "UDP",
				state:      GameServerStatePortAllocation,
				policy:     Dynamic,
				scheduling: Packed,
				health: Health{
					Disabled:            false,
					FailureThreshold:    3,
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
				},
			},
		},
		"defaults are already set": {
			gameServer: GameServer{
				Spec: GameServerSpec{
					Container: "testing2",
					Ports: []GameServerPort{{
						Protocol:   "TCP",
						PortPolicy: Static,
					}},
					Health: Health{
						Disabled:            false,
						PeriodSeconds:       12,
						InitialDelaySeconds: 11,
						FailureThreshold:    10,
					},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "testing", Image: "testing/image"},
								{Name: "testing2", Image: "testing/image2"}}},
					},
				},
				Status: GameServerStatus{State: "TestState"}},
			container: "testing2",
			expected: expected{
				protocol:   "TCP",
				state:      "TestState",
				policy:     Static,
				scheduling: Packed,
				health: Health{
					Disabled:            false,
					FailureThreshold:    10,
					InitialDelaySeconds: 11,
					PeriodSeconds:       12,
				},
			},
		},
		"set basic defaults on static gameserver": {
			gameServer: GameServer{
				Spec: GameServerSpec{
					Ports: []GameServerPort{{PortPolicy: Static}},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "testing", Image: "testing/image"}}}}},
			},
			container: "testing",
			expected: expected{
				protocol:   "UDP",
				state:      GameServerStateCreating,
				policy:     Static,
				scheduling: Packed,
				health: Health{
					Disabled:            false,
					FailureThreshold:    3,
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
				},
			},
		},
		"health is disabled": {
			gameServer: GameServer{
				Spec: GameServerSpec{
					Ports:  []GameServerPort{{ContainerPort: 999}},
					Health: Health{Disabled: true},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "testing", Image: "testing/image"}}}}},
			},
			container: "testing",
			expected: expected{
				protocol:   "UDP",
				state:      GameServerStatePortAllocation,
				policy:     Dynamic,
				scheduling: Packed,
				health: Health{
					Disabled: true,
				},
			},
		},
		"convert from legacy single port to multiple": {
			gameServer: GameServer{
				Spec: GameServerSpec{
					Ports: []GameServerPort{
						{
							ContainerPort: 777,
							HostPort:      777,
							PortPolicy:    Static,
							Protocol:      corev1.ProtocolTCP,
						},
					},
					Health: Health{Disabled: true},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "testing", Image: "testing/image"}}}}},
			},
			container: "testing",
			expected: expected{
				protocol:   corev1.ProtocolTCP,
				state:      GameServerStateCreating,
				policy:     Static,
				scheduling: Packed,
				health:     Health{Disabled: true},
			},
		},
	}

	for name, test := range data {
		t.Run(name, func(t *testing.T) {
			test.gameServer.ApplyDefaults()

			spec := test.gameServer.Spec
			assert.Contains(t, test.gameServer.ObjectMeta.Finalizers, stable.GroupName)
			assert.Equal(t, test.container, spec.Container)
			assert.Equal(t, test.expected.protocol, spec.Ports[0].Protocol)
			assert.Equal(t, test.expected.state, test.gameServer.Status.State)
			assert.Equal(t, test.expected.health, test.gameServer.Spec.Health)
			assert.Equal(t, test.expected.scheduling, test.gameServer.Spec.Scheduling)
		})
	}
}

func TestGameServerValidate(t *testing.T) {
	gs := GameServer{
		Spec: GameServerSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "testing", Image: "testing/image"}}}}},
	}
	gs.ApplyDefaults()
	ok, causes := gs.Validate()
	assert.True(t, ok)
	assert.Empty(t, causes)

	gs = GameServer{
		Spec: GameServerSpec{
			Container: "",
			Ports: []GameServerPort{{
				Name:       "main",
				HostPort:   5001,
				PortPolicy: Dynamic,
			}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "testing", Image: "testing/image"},
					{Name: "anothertest", Image: "testing/image"},
				}}}},
	}
	ok, causes = gs.Validate()
	var fields []string
	for _, f := range causes {
		fields = append(fields, f.Field)
	}
	assert.False(t, ok)
	assert.Len(t, causes, 3)
	assert.Contains(t, fields, "container")
	assert.Contains(t, fields, "main.hostPort")
	assert.Equal(t, causes[0].Type, metav1.CauseTypeFieldValueInvalid)

	gs = GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "dev-game",
			Namespace:   "default",
			Annotations: map[string]string{DevAddressAnnotation: "invalid-ip"},
		},
		Spec: GameServerSpec{
			Ports: []GameServerPort{{Name: "main", ContainerPort: 7777, PortPolicy: Static}},
		},
	}
	ok, causes = gs.Validate()
	for _, f := range causes {
		fields = append(fields, f.Field)
	}
	assert.False(t, ok)
	assert.Len(t, causes, 2)
	assert.Contains(t, fields, fmt.Sprintf("annotations.%s", DevAddressAnnotation))
	assert.Contains(t, fields, "main.hostPort")
	assert.Equal(t, causes[1].Type, metav1.CauseTypeFieldValueRequired)
}

func TestGameServerPod(t *testing.T) {
	fixture := &GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "1234"},
		Spec: GameServerSpec{
			Ports: []GameServerPort{
				{
					ContainerPort: 7777,
					HostPort:      9999,
					PortPolicy:    Static,
				},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "container", Image: "container/image"}},
				},
			},
		}, Status: GameServerStatus{State: GameServerStateCreating}}
	fixture.ApplyDefaults()

	pod, err := fixture.Pod()
	assert.Nil(t, err, "Pod should not return an error")
	assert.Equal(t, fixture.ObjectMeta.Name, pod.ObjectMeta.Name)
	assert.Equal(t, fixture.ObjectMeta.Namespace, pod.ObjectMeta.Namespace)
	assert.Equal(t, "gameserver", pod.ObjectMeta.Labels[stable.GroupName+"/role"])
	assert.Equal(t, fixture.ObjectMeta.Name, pod.ObjectMeta.Labels[GameServerPodLabel])
	assert.Equal(t, fixture.Spec.Container, pod.ObjectMeta.Annotations[GameServerContainerAnnotation])
	assert.Equal(t, "agones-sdk", pod.Spec.ServiceAccountName)
	assert.True(t, metav1.IsControlledBy(pod, fixture))
	assert.Equal(t, fixture.Spec.Ports[0].HostPort, pod.Spec.Containers[0].Ports[0].HostPort)
	assert.Equal(t, fixture.Spec.Ports[0].ContainerPort, pod.Spec.Containers[0].Ports[0].ContainerPort)
	assert.Equal(t, corev1.Protocol("UDP"), pod.Spec.Containers[0].Ports[0].Protocol)
	assert.True(t, metav1.IsControlledBy(pod, fixture))

	sidecar := corev1.Container{Name: "sidecar", Image: "container/sidecar"}
	fixture.Spec.Template.Spec.ServiceAccountName = "other-agones-sdk"
	pod, err = fixture.Pod(sidecar)
	assert.Nil(t, err, "Pod should not return an error")
	assert.Equal(t, fixture.ObjectMeta.Name, pod.ObjectMeta.Name)
	assert.Len(t, pod.Spec.Containers, 2, "Should have two containers")
	assert.Equal(t, "other-agones-sdk", pod.Spec.ServiceAccountName)
	assert.Equal(t, "container", pod.Spec.Containers[0].Name)
	assert.Equal(t, "sidecar", pod.Spec.Containers[1].Name)
	assert.True(t, metav1.IsControlledBy(pod, fixture))
}

func TestGameServerPodObjectMeta(t *testing.T) {
	fixture := &GameServer{ObjectMeta: metav1.ObjectMeta{Name: "lucy"},
		Spec: GameServerSpec{Container: "goat"}}

	f := func(t *testing.T, gs *GameServer, pod *corev1.Pod) {
		assert.Equal(t, gs.ObjectMeta.Name, pod.ObjectMeta.Name)
		assert.Equal(t, gs.ObjectMeta.Namespace, pod.ObjectMeta.Namespace)
		assert.Equal(t, GameServerLabelRole, pod.ObjectMeta.Labels[RoleLabel])
		assert.Equal(t, "gameserver", pod.ObjectMeta.Labels[stable.GroupName+"/role"])
		assert.Equal(t, gs.ObjectMeta.Name, pod.ObjectMeta.Labels[GameServerPodLabel])
		assert.Equal(t, "goat", pod.ObjectMeta.Annotations[GameServerContainerAnnotation])
		assert.True(t, metav1.IsControlledBy(pod, gs))
	}

	t.Run("packed", func(t *testing.T) {
		gs := fixture.DeepCopy()
		gs.Spec.Scheduling = Packed
		pod := &corev1.Pod{}

		gs.podObjectMeta(pod)
		f(t, gs, pod)

		assert.Equal(t, "false", pod.ObjectMeta.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"])
	})

	t.Run("distributed", func(t *testing.T) {
		gs := fixture.DeepCopy()
		gs.Spec.Scheduling = Distributed
		pod := &corev1.Pod{}

		gs.podObjectMeta(pod)
		f(t, gs, pod)

		assert.Equal(t, "", pod.ObjectMeta.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"])
	})
}

func TestGameServerPodScheduling(t *testing.T) {
	fixture := &corev1.Pod{Spec: corev1.PodSpec{}}

	t.Run("packed", func(t *testing.T) {
		gs := &GameServer{Spec: GameServerSpec{Scheduling: Packed}}
		pod := fixture.DeepCopy()
		gs.podScheduling(pod)

		assert.Len(t, pod.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution, 1)
		wpat := pod.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0]
		assert.Equal(t, int32(100), wpat.Weight)
		assert.Contains(t, wpat.PodAffinityTerm.LabelSelector.String(), GameServerLabelRole)
		assert.Contains(t, wpat.PodAffinityTerm.LabelSelector.String(), RoleLabel)
	})

	t.Run("distributed", func(t *testing.T) {
		gs := &GameServer{Spec: GameServerSpec{Scheduling: Distributed}}
		pod := fixture.DeepCopy()
		gs.podScheduling(pod)
		assert.Empty(t, pod.Spec.Affinity)
	})
}

func TestGameServerCountPorts(t *testing.T) {
	fixture := &GameServer{Spec: GameServerSpec{Ports: []GameServerPort{
		{PortPolicy: Dynamic},
		{PortPolicy: Dynamic},
		{PortPolicy: Dynamic},
		{PortPolicy: Static},
	}}}

	assert.Equal(t, 3, fixture.CountPorts(Dynamic))
	assert.Equal(t, 1, fixture.CountPorts(Static))
}

func TestGameServerPatch(t *testing.T) {
	fixture := &GameServer{ObjectMeta: metav1.ObjectMeta{Name: "lucy"},
		Spec: GameServerSpec{Container: "goat"}}

	delta := fixture.DeepCopy()
	delta.Spec.Container = "bear"

	patch, err := fixture.Patch(delta)
	assert.Nil(t, err)

	assert.Contains(t, string(patch), `{"op":"replace","path":"/spec/container","value":"bear"}`)
}
func TestGetDevAddress(t *testing.T) {
	devGs := &GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "dev-game",
			Namespace:   "default",
			Annotations: map[string]string{DevAddressAnnotation: ipFixture},
		},
		Spec: GameServerSpec{
			Ports: []GameServerPort{{HostPort: 7777, PortPolicy: Static}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "container", Image: "container/image"}},
			},
			},
		},
	}

	devAddress, isDev := devGs.GetDevAddress()
	assert.True(t, isDev, "dev-game should had a dev-address")
	assert.Equal(t, ipFixture, devAddress, "dev-address IP address should be 127.1.1.1")

	regularGs := devGs.DeepCopy()
	regularGs.ObjectMeta.Annotations = map[string]string{}
	devAddress, isDev = regularGs.GetDevAddress()
	assert.False(t, isDev, "dev-game should NOT have a dev-address")
	assert.Equal(t, "", devAddress, "dev-address IP address should be 127.1.1.1")
}
