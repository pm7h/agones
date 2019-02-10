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

package gameserversets

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	agtesting "agones.dev/agones/pkg/testing"
	"agones.dev/agones/pkg/util/webhooks"
	"github.com/heptiolabs/healthcheck"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func gsWithState(st v1alpha1.GameServerState) *v1alpha1.GameServer {
	return &v1alpha1.GameServer{Status: v1alpha1.GameServerStatus{State: st}}
}

func gsPendingDeletionWithState(st v1alpha1.GameServerState) *v1alpha1.GameServer {
	return &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			DeletionTimestamp: &deletionTime,
		},
		Status: v1alpha1.GameServerStatus{State: st},
	}
}

const (
	maxTestCreationsPerBatch = 3
	maxTestDeletionsPerBatch = 3
	maxTestPendingPerBatch   = 3
)

func TestComputeReconciliationAction(t *testing.T) {
	cases := []struct {
		desc                   string
		list                   []*v1alpha1.GameServer
		targetReplicaCount     int
		wantNumServersToAdd    int
		wantNumServersToDelete int
		wantIsPartial          bool
	}{
		{
			desc: "Empty",
		},
		{
			desc: "AddServers",
			list: []*v1alpha1.GameServer{
				gsWithState(v1alpha1.GameServerStateReady),
			},
			targetReplicaCount:  3,
			wantNumServersToAdd: 2,
		},
		{
			// 1 ready servers, target is 30 but we can only create 3 at a time.
			desc: "AddServersPartial",
			list: []*v1alpha1.GameServer{
				gsWithState(v1alpha1.GameServerStateReady),
			},
			targetReplicaCount:  30,
			wantNumServersToAdd: 3,
			wantIsPartial:       true, // max 3 creations per action
		},
		{
			// 0 ready servers, target is 30 but we can only have 3 in-flight.
			desc: "AddServersExceedsInFlightLimit",
			list: []*v1alpha1.GameServer{
				gsWithState(v1alpha1.GameServerStateCreating),
				gsWithState(v1alpha1.GameServerStatePortAllocation),
			},
			targetReplicaCount:  30,
			wantNumServersToAdd: 1,
			wantIsPartial:       true,
		}, {
			desc: "DeleteServers",
			list: []*v1alpha1.GameServer{
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
			},
			targetReplicaCount:     1,
			wantNumServersToDelete: 2,
		},
		{
			// 6 ready servers, target is 1 but we can only delete 3 at a time.
			desc: "DeleteServerPartial",
			list: []*v1alpha1.GameServer{
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateReady),
			},
			targetReplicaCount:     1,
			wantNumServersToDelete: 3,
			wantIsPartial:          true, // max 3 deletions per action
		},
		{
			desc: "DeleteIgnoresAllocatedServers",
			list: []*v1alpha1.GameServer{
				gsWithState(v1alpha1.GameServerStateReady),
				gsWithState(v1alpha1.GameServerStateAllocated),
				gsWithState(v1alpha1.GameServerStateAllocated),
			},
			targetReplicaCount:     0,
			wantNumServersToDelete: 1,
		},
		{
			desc: "CreateWhileDeletionsPending",
			list: []*v1alpha1.GameServer{
				// 2 being deleted, one ready, target is 4, we add 3 more.
				gsPendingDeletionWithState(v1alpha1.GameServerStateUnhealthy),
				gsPendingDeletionWithState(v1alpha1.GameServerStateUnhealthy),
				gsWithState(v1alpha1.GameServerStateReady),
			},
			targetReplicaCount:  4,
			wantNumServersToAdd: 3,
		},
		{
			desc: "PendingDeletionsCountTowardsTargetReplicaCount",
			list: []*v1alpha1.GameServer{
				// 6 being deleted now, we want 10 but that would exceed in-flight limit by a lot.
				gsWithState(v1alpha1.GameServerStateCreating),
				gsWithState(v1alpha1.GameServerStatePortAllocation),
				gsWithState(v1alpha1.GameServerStateCreating),
				gsWithState(v1alpha1.GameServerStatePortAllocation),
				gsWithState(v1alpha1.GameServerStateCreating),
				gsWithState(v1alpha1.GameServerStatePortAllocation),
			},
			targetReplicaCount:  10,
			wantNumServersToAdd: 0,
			wantIsPartial:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			toAdd, toDelete, isPartial := computeReconciliationAction(tc.list, tc.targetReplicaCount, maxTestCreationsPerBatch, maxTestDeletionsPerBatch, maxTestPendingPerBatch)

			assert.Equal(t, tc.wantNumServersToAdd, toAdd, "# of GameServers to add")
			assert.Len(t, toDelete, tc.wantNumServersToDelete, "# of GameServers to delete")
			assert.Equal(t, tc.wantIsPartial, isPartial, "is partial reconciliation")
		})
	}
}

func TestComputeStatus(t *testing.T) {
	cases := []struct {
		list       []*v1alpha1.GameServer
		wantStatus v1alpha1.GameServerSetStatus
	}{
		{[]*v1alpha1.GameServer{}, v1alpha1.GameServerSetStatus{}},
		{[]*v1alpha1.GameServer{
			gsWithState(v1alpha1.GameServerStateCreating),
			gsWithState(v1alpha1.GameServerStateReady),
		}, v1alpha1.GameServerSetStatus{ReadyReplicas: 1, Replicas: 2}},
		{[]*v1alpha1.GameServer{
			gsWithState(v1alpha1.GameServerStateAllocated),
			gsWithState(v1alpha1.GameServerStateAllocated),
			gsWithState(v1alpha1.GameServerStateCreating),
			gsWithState(v1alpha1.GameServerStateReady),
		}, v1alpha1.GameServerSetStatus{ReadyReplicas: 1, AllocatedReplicas: 2, Replicas: 4}},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.wantStatus, computeStatus(tc.list))
	}
}

func TestControllerWatchGameServers(t *testing.T) {
	gsSet := defaultFixture()

	c, m := newFakeController()

	received := make(chan string)
	defer close(received)

	m.ExtClient.AddReactor("get", "customresourcedefinitions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, agtesting.NewEstablishedCRD(), nil
	})
	gsSetWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameserversets", k8stesting.DefaultWatchReactor(gsSetWatch, nil))
	gsWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))

	c.workerqueue.SyncHandler = func(name string) error {
		received <- name
		return nil
	}

	stop, cancel := agtesting.StartInformers(m, c.gameServerSynced)
	defer cancel()

	go func() {
		err := c.Run(1, stop)
		assert.Nil(t, err)
	}()

	f := func() string {
		select {
		case result := <-received:
			return result
		case <-time.After(3 * time.Second):
			assert.FailNow(t, "timeout occurred")
		}
		return ""
	}

	expected, err := cache.MetaNamespaceKeyFunc(gsSet)
	assert.Nil(t, err)

	// gsSet add
	logrus.Info("adding gsSet")
	gsSetWatch.Add(gsSet.DeepCopy())
	assert.Nil(t, err)
	assert.Equal(t, expected, f())
	// gsSet update
	logrus.Info("modify gsSet")
	gsSetCopy := gsSet.DeepCopy()
	gsSetCopy.Spec.Replicas = 5
	gsSetWatch.Modify(gsSetCopy)
	assert.Equal(t, expected, f())

	gs := gsSet.GameServer()
	gs.ObjectMeta.Name = "test-gs"
	// gs add
	logrus.Info("add gs")
	gsWatch.Add(gs.DeepCopy())
	assert.Equal(t, expected, f())

	// gs update
	gsCopy := gs.DeepCopy()
	now := metav1.Now()
	gsCopy.ObjectMeta.DeletionTimestamp = &now

	logrus.Info("modify gs - noop")
	gsWatch.Modify(gsCopy.DeepCopy())
	select {
	case <-received:
		assert.Fail(t, "Should be no value")
	case <-time.After(time.Second):
	}

	gsCopy = gs.DeepCopy()
	gsCopy.Status.State = v1alpha1.GameServerStateUnhealthy
	logrus.Info("modify gs - unhealthy")
	gsWatch.Modify(gsCopy.DeepCopy())
	assert.Equal(t, expected, f())

	// gs delete
	logrus.Info("delete gs")
	gsWatch.Delete(gsCopy.DeepCopy())
	assert.Equal(t, expected, f())
}

func TestSyncGameServerSet(t *testing.T) {
	t.Run("adding and deleting unhealthy gameservers", func(t *testing.T) {
		gsSet := defaultFixture()
		list := createGameServers(gsSet, 5)

		// make some as unhealthy
		list[0].Status.State = v1alpha1.GameServerStateUnhealthy

		updated := false
		count := 0

		c, m := newFakeController()
		m.AgonesClient.AddReactor("list", "gameserversets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.GameServerSetList{Items: []v1alpha1.GameServerSet{*gsSet}}, nil
		})
		m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.GameServerList{Items: list}, nil
		})

		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, gs.Status.State, v1alpha1.GameServerStateShutdown)

			updated = true
			assert.Equal(t, "test-0", gs.GetName())
			return true, nil, nil
		})
		m.AgonesClient.AddReactor("create", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			ca := action.(k8stesting.CreateAction)
			gs := ca.GetObject().(*v1alpha1.GameServer)

			assert.True(t, metav1.IsControlledBy(gs, gsSet))
			count++
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(m, c.gameServerSetSynced, c.gameServerSynced)
		defer cancel()

		c.syncGameServerSet(gsSet.ObjectMeta.Namespace + "/" + gsSet.ObjectMeta.Name) // nolint: errcheck

		assert.Equal(t, 6, count)
		assert.True(t, updated, "A game servers should have been updated")
	})

	t.Run("removing gamservers", func(t *testing.T) {
		gsSet := defaultFixture()
		list := createGameServers(gsSet, 15)
		count := 0

		c, m := newFakeController()
		m.AgonesClient.AddReactor("list", "gameserversets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.GameServerSetList{Items: []v1alpha1.GameServerSet{*gsSet}}, nil
		})
		m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.GameServerList{Items: list}, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			count++
			return true, nil, nil
		})

		_, cancel := agtesting.StartInformers(m, c.gameServerSetSynced, c.gameServerSynced)
		defer cancel()

		c.syncGameServerSet(gsSet.ObjectMeta.Namespace + "/" + gsSet.ObjectMeta.Name) // nolint: errcheck

		assert.Equal(t, 5, count)
	})
}

func TestControllerSyncUnhealthyGameServers(t *testing.T) {
	gsSet := defaultFixture()

	gs1 := gsSet.GameServer()
	gs1.ObjectMeta.Name = "test-1"
	gs1.Status = v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateUnhealthy}

	gs2 := gsSet.GameServer()
	gs2.ObjectMeta.Name = "test-2"
	gs2.Status = v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}

	gs3 := gsSet.GameServer()
	gs3.ObjectMeta.Name = "test-3"
	now := metav1.Now()
	gs3.ObjectMeta.DeletionTimestamp = &now
	gs3.Status = v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}

	var updatedCount int

	c, m := newFakeController()
	m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua := action.(k8stesting.UpdateAction)
		gs := ua.GetObject().(*v1alpha1.GameServer)

		assert.Equal(t, gs.Status.State, v1alpha1.GameServerStateShutdown)

		updatedCount++
		return true, nil, nil
	})

	_, cancel := agtesting.StartInformers(m)
	defer cancel()

	err := c.deleteGameServers(gsSet, []*v1alpha1.GameServer{gs1, gs2, gs3})
	assert.Nil(t, err)

	assert.Equal(t, 3, updatedCount, "Updates should have occured")
}

func TestSyncMoreGameServers(t *testing.T) {
	gsSet := defaultFixture()

	c, m := newFakeController()
	count := 0
	expected := 10

	m.AgonesClient.AddReactor("create", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca := action.(k8stesting.CreateAction)
		gs := ca.GetObject().(*v1alpha1.GameServer)

		assert.True(t, metav1.IsControlledBy(gs, gsSet))
		count++

		return true, gs, nil
	})

	_, cancel := agtesting.StartInformers(m)
	defer cancel()

	err := c.addMoreGameServers(gsSet, expected)
	assert.Nil(t, err)
	assert.Equal(t, expected, count)
	agtesting.AssertEventContains(t, m.FakeRecorder.Events, "SuccessfulCreate")
}

func TestControllerSyncGameServerSetStatus(t *testing.T) {
	t.Parallel()

	t.Run("all ready list", func(t *testing.T) {
		gsSet := defaultFixture()
		c, m := newFakeController()

		updated := false
		m.AgonesClient.AddReactor("update", "gameserversets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true
			ua := action.(k8stesting.UpdateAction)
			gsSet := ua.GetObject().(*v1alpha1.GameServerSet)

			assert.Equal(t, int32(1), gsSet.Status.Replicas)
			assert.Equal(t, int32(1), gsSet.Status.ReadyReplicas)
			assert.Equal(t, int32(0), gsSet.Status.AllocatedReplicas)

			return true, nil, nil
		})

		list := []*v1alpha1.GameServer{{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}}}
		err := c.syncGameServerSetStatus(gsSet, list)
		assert.Nil(t, err)
		assert.True(t, updated)
	})

	t.Run("only some ready list", func(t *testing.T) {
		gsSet := defaultFixture()
		c, m := newFakeController()

		updated := false
		m.AgonesClient.AddReactor("update", "gameserversets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true
			ua := action.(k8stesting.UpdateAction)
			gsSet := ua.GetObject().(*v1alpha1.GameServerSet)

			assert.Equal(t, int32(8), gsSet.Status.Replicas)
			assert.Equal(t, int32(1), gsSet.Status.ReadyReplicas)
			assert.Equal(t, int32(2), gsSet.Status.AllocatedReplicas)

			return true, nil, nil
		})

		list := []*v1alpha1.GameServer{
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateStarting}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateUnhealthy}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStatePortAllocation}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateError}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateCreating}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateAllocated}},
			{Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateAllocated}},
		}
		err := c.syncGameServerSetStatus(gsSet, list)
		assert.Nil(t, err)
		assert.True(t, updated)
	})
}

func TestControllerUpdateValidationHandler(t *testing.T) {
	t.Parallel()

	c, _ := newFakeController()
	gvk := metav1.GroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("GameServerSet"))
	fixture := &v1alpha1.GameServerSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.GameServerSetSpec{Replicas: 5},
	}
	raw, err := json.Marshal(fixture)
	assert.Nil(t, err)

	t.Run("valid gameserverset update", func(t *testing.T) {
		new := fixture.DeepCopy()
		new.Spec.Replicas = 10
		newRaw, err := json.Marshal(new)
		assert.Nil(t, err)

		review := admv1beta1.AdmissionReview{
			Request: &admv1beta1.AdmissionRequest{
				Kind:      gvk,
				Operation: admv1beta1.Create,
				Object: runtime.RawExtension{
					Raw: newRaw,
				},
				OldObject: runtime.RawExtension{
					Raw: raw,
				},
			},
			Response: &admv1beta1.AdmissionResponse{Allowed: true},
		}

		result, err := c.updateValidationHandler(review)
		assert.Nil(t, err)
		assert.True(t, result.Response.Allowed)
	})

	t.Run("invalid gameserverset update", func(t *testing.T) {
		new := fixture.DeepCopy()
		new.Spec.Template = v1alpha1.GameServerTemplateSpec{
			Spec: v1alpha1.GameServerSpec{
				Ports: []v1alpha1.GameServerPort{{PortPolicy: v1alpha1.Static}},
			},
		}
		newRaw, err := json.Marshal(new)
		assert.Nil(t, err)

		assert.NotEqual(t, string(raw), string(newRaw))

		review := admv1beta1.AdmissionReview{
			Request: &admv1beta1.AdmissionRequest{
				Kind:      gvk,
				Operation: admv1beta1.Create,
				Object: runtime.RawExtension{
					Raw: newRaw,
				},
				OldObject: runtime.RawExtension{
					Raw: raw,
				},
			},
			Response: &admv1beta1.AdmissionResponse{Allowed: true},
		}

		logrus.Info("here?")
		result, err := c.updateValidationHandler(review)
		assert.Nil(t, err)
		assert.False(t, result.Response.Allowed)
		assert.Equal(t, metav1.StatusFailure, result.Response.Result.Status)
		assert.Equal(t, metav1.StatusReasonInvalid, result.Response.Result.Reason)
	})
}

// defaultFixture creates the default GameServerSet fixture
func defaultFixture() *v1alpha1.GameServerSet {
	gsSet := &v1alpha1.GameServerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test", UID: "1234"},
		Spec: v1alpha1.GameServerSetSpec{
			Replicas:   10,
			Scheduling: v1alpha1.Packed,
			Template:   v1alpha1.GameServerTemplateSpec{},
		},
	}
	return gsSet
}

// createGameServers create an array of GameServers from the GameServerSet
func createGameServers(gsSet *v1alpha1.GameServerSet, size int) []v1alpha1.GameServer {
	var list []v1alpha1.GameServer
	for i := 0; i < size; i++ {
		gs := gsSet.GameServer()
		gs.Name = gs.GenerateName + strconv.Itoa(i)
		gs.Status = v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}
		list = append(list, *gs)
	}
	return list
}

// newFakeController returns a controller, backed by the fake Clientset
func newFakeController() (*Controller, agtesting.Mocks) {
	m := agtesting.NewMocks()
	wh := webhooks.NewWebHook("", "")
	c := NewController(wh, healthcheck.NewHandler(), m.KubeClient, m.ExtClient, m.AgonesClient, m.AgonesInformerFactory)
	c.recorder = m.FakeRecorder
	return c, m
}
