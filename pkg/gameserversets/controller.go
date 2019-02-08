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
	"sort"
	"sync"

	"agones.dev/agones/pkg/apis/stable"
	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	getterv1alpha1 "agones.dev/agones/pkg/client/clientset/versioned/typed/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/crd"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/webhooks"
	"agones.dev/agones/pkg/util/workerqueue"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

var (
	// ErrNoGameServerSetOwner is returned when a GameServerSet can't be found as an owner
	// for a GameServer
	ErrNoGameServerSetOwner = errors.New("No GameServerSet owner for this GameServer")
)

const (
	maxCreationParalellism         = 16
	maxGameServerCreationsPerBatch = 64

	maxDeletionParallelism         = 64
	maxGameServerDeletionsPerBatch = 64

	// maxPodPendingCount is the maximum number of pending pods per game server set
	maxPodPendingCount = 5000
)

// Controller is a the GameServerSet controller
type Controller struct {
	logger              *logrus.Entry
	crdGetter           v1beta1.CustomResourceDefinitionInterface
	gameServerGetter    getterv1alpha1.GameServersGetter
	gameServerLister    listerv1alpha1.GameServerLister
	gameServerSynced    cache.InformerSynced
	gameServerSetGetter getterv1alpha1.GameServerSetsGetter
	gameServerSetLister listerv1alpha1.GameServerSetLister
	gameServerSetSynced cache.InformerSynced
	workerqueue         *workerqueue.WorkerQueue
	allocationMutex     *sync.Mutex
	stop                <-chan struct{}
	recorder            record.EventRecorder
	stateCache          *gameServerStateCache
}

// NewController returns a new gameserverset crd controller
func NewController(
	wh *webhooks.WebHook,
	health healthcheck.Handler,
	allocationMutex *sync.Mutex,
	kubeClient kubernetes.Interface,
	extClient extclientset.Interface,
	agonesClient versioned.Interface,
	agonesInformerFactory externalversions.SharedInformerFactory) *Controller {

	gameServers := agonesInformerFactory.Stable().V1alpha1().GameServers()
	gsInformer := gameServers.Informer()
	gameServerSets := agonesInformerFactory.Stable().V1alpha1().GameServerSets()
	gsSetInformer := gameServerSets.Informer()

	c := &Controller{
		crdGetter:           extClient.ApiextensionsV1beta1().CustomResourceDefinitions(),
		gameServerGetter:    agonesClient.StableV1alpha1(),
		gameServerLister:    gameServers.Lister(),
		gameServerSynced:    gsInformer.HasSynced,
		gameServerSetGetter: agonesClient.StableV1alpha1(),
		gameServerSetLister: gameServerSets.Lister(),
		gameServerSetSynced: gsSetInformer.HasSynced,
		allocationMutex:     allocationMutex,
		stateCache:          &gameServerStateCache{},
	}

	c.logger = runtime.NewLoggerWithType(c)
	c.workerqueue = workerqueue.NewWorkerQueue(c.syncGameServerSet, c.logger, stable.GroupName+".GameServerSetController")
	health.AddLivenessCheck("gameserverset-workerqueue", healthcheck.Check(c.workerqueue.Healthy))

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(c.logger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	c.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "gameserverset-controller"})

	wh.AddHandler("/validate", v1alpha1.Kind("GameServerSet"), admv1beta1.Update, c.updateValidationHandler)

	gsSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.workerqueue.Enqueue,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldGss := oldObj.(*v1alpha1.GameServerSet)
			newGss := newObj.(*v1alpha1.GameServerSet)
			if oldGss.Spec.Replicas != newGss.Spec.Replicas {
				c.workerqueue.Enqueue(newGss)
			}
		},
		DeleteFunc: func(gsSet interface{}) {
			c.stateCache.deleteGameServerSet(gsSet.(*v1alpha1.GameServerSet))
		},
	})

	gsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.gameServerEventHandler,
		UpdateFunc: func(oldObj, newObj interface{}) {
			gs := newObj.(*v1alpha1.GameServer)
			// ignore if already being deleted
			if gs.ObjectMeta.DeletionTimestamp == nil {
				c.gameServerEventHandler(gs)
			}
		},
		DeleteFunc: c.gameServerEventHandler,
	})

	return c
}

// Run the GameServerSet controller. Will block until stop is closed.
// Runs threadiness number workers to process the rate limited queue
func (c *Controller) Run(workers int, stop <-chan struct{}) error {
	c.stop = stop

	err := crd.WaitForEstablishedCRD(c.crdGetter, "gameserversets."+stable.GroupName, c.logger)
	if err != nil {
		return err
	}

	c.logger.Info("Wait for cache sync")
	if !cache.WaitForCacheSync(stop, c.gameServerSynced, c.gameServerSetSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	c.workerqueue.Run(workers, stop)
	return nil
}

// updateValidationHandler that validates a GameServerSet when is updated
// Should only be called on gameserverset update operations.
func (c *Controller) updateValidationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	c.logger.WithField("review", review).Info("updateValidationHandler")

	newGss := &v1alpha1.GameServerSet{}
	oldGss := &v1alpha1.GameServerSet{}

	newObj := review.Request.Object
	if err := json.Unmarshal(newObj.Raw, newGss); err != nil {
		return review, errors.Wrapf(err, "error unmarshalling new GameServerSet json: %s", newObj.Raw)
	}

	oldObj := review.Request.OldObject
	if err := json.Unmarshal(oldObj.Raw, oldGss); err != nil {
		return review, errors.Wrapf(err, "error unmarshalling old GameServerSet json: %s", oldObj.Raw)
	}

	ok, causes := oldGss.ValidateUpdate(newGss)
	if !ok {
		review.Response.Allowed = false
		details := metav1.StatusDetails{
			Name:   review.Request.Name,
			Group:  review.Request.Kind.Group,
			Kind:   review.Request.Kind.Kind,
			Causes: causes,
		}
		review.Response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "GameServer update is invalid",
			Reason:  metav1.StatusReasonInvalid,
			Details: &details,
		}

		c.logger.WithField("review", review).Info("Invalid GameServerSet update")
		return review, nil
	}

	return review, nil
}

func (c *Controller) gameServerEventHandler(obj interface{}) {
	gs, ok := obj.(*v1alpha1.GameServer)
	if !ok {
		return
	}

	ref := metav1.GetControllerOf(gs)
	if ref == nil {
		return
	}
	gsSet, err := c.gameServerSetLister.GameServerSets(gs.ObjectMeta.Namespace).Get(ref.Name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("ref", ref).Info("Owner GameServerSet no longer available for syncing")
		} else {
			runtime.HandleError(c.logger.WithField("gs", gs.ObjectMeta.Name).WithField("ref", ref),
				errors.Wrap(err, "error retrieving GameServer owner"))
		}
		return
	}
	c.workerqueue.EnqueueImmediately(gsSet)
}

// syncGameServer synchronises the GameServers for the Set,
// making sure there are aways as many GameServers as requested
func (c *Controller) syncGameServerSet(key string) error {
	c.logger.WithField("key", key).Info("syncGameServerSet")
	defer c.logger.WithField("key", key).Info("syncGameServerSet finished")

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// don't return an error, as we don't want this retried
		runtime.HandleError(c.logger.WithField("key", key), errors.Wrapf(err, "invalid resource key"))
		return nil
	}

	gsSet, err := c.gameServerSetLister.GameServerSets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("key", key).Info("GameServerSet is no longer available for syncing")
			return nil
		}
		return errors.Wrapf(err, "error retrieving GameServerSet %s from namespace %s", name, namespace)
	}

	list, err := ListGameServersByGameServerSetOwner(c.gameServerLister, gsSet)
	if err != nil {
		return err
	}

	list = c.stateCache.forGameServerSet(gsSet).reconcileWithUpdatedServerList(list)

	numServersToAdd, toDelete, isPartial := computeReconciliationAction(list, int(gsSet.Spec.Replicas), maxGameServerCreationsPerBatch, maxGameServerDeletionsPerBatch, maxPodPendingCount)
	status := computeStatus(list)
	fields := logrus.Fields{}

	for _, gs := range list {
		key := "gsCount" + string(gs.Status.State)
		if gs.ObjectMeta.DeletionTimestamp != nil {
			key = key + "Deleted"
		}
		v, ok := fields[key]
		if !ok {
			v = 0
		}

		fields[key] = v.(int) + 1
	}
	c.logger.
		WithField("targetReplicaCount", gsSet.Spec.Replicas).
		WithField("numServersToAdd", numServersToAdd).
		WithField("numServersToDelete", len(toDelete)).
		WithField("isPartial", isPartial).
		WithField("status", status).
		WithFields(fields).
		Info("Reconciling GameServerSet")
	if isPartial {
		// we've determined that there's work to do, but we've decided not to do all the work in one shot
		// make sure we get a follow-up, by re-scheduling this GSS in the worker queue immediately before this
		// function returns
		defer c.workerqueue.EnqueueImmediately(gsSet)
	}

	if numServersToAdd > 0 {
		if err := c.addMoreGameServers(gsSet, numServersToAdd); err != nil {
			c.logger.WithError(err).Warning("error adding game servers")
		}
	}

	if len(toDelete) > 0 {
		if err := c.deleteGameServers(gsSet, toDelete); err != nil {
			c.logger.WithError(err).Warning("error deleting game servers")
		}
	}

	return c.syncGameServerSetStatus(gsSet, list)
}

// computeReconciliationAction computes the action to take to reconcile a game server set set given
// the list of game servers that were found and target replica count.
func computeReconciliationAction(list []*v1alpha1.GameServer, targetReplicaCount int, maxCreations int, maxDeletions int, maxPending int) (int, []*v1alpha1.GameServer, bool) {
	var upCount int // up == Ready or will become ready

	// track the number of pods that are being created at any given moment by the GameServerSet
	// so we can limit it at a throughput that Kubernetes can handle
	var podPendingCount int // podPending == "up" but don't have a Pod running yet
	var toDelete []*v1alpha1.GameServer

	scheduleDeletion := func(gs *v1alpha1.GameServer) {
		if gs.ObjectMeta.DeletionTimestamp.IsZero() {
			toDelete = append(toDelete, gs)
		}
	}

	handleGameServerUp := func(gs *v1alpha1.GameServer) {
		if upCount >= targetReplicaCount {
			scheduleDeletion(gs)
		} else {
			upCount++
		}
	}

	// pass 1 - count allocated servers only, since those can't be touched
	for _, gs := range list {
		if isAllocated(gs) {
			upCount++
			continue
		}
	}

	// pass 2 - handle all other statuses
	for _, gs := range list {
		if isAllocated(gs) {
			// already handled above
			continue
		}

		// GS being deleted counts towards target replica count.
		if !gs.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}

		switch gs.Status.State {
		case v1alpha1.GameServerStatePortAllocation:
			podPendingCount++
			handleGameServerUp(gs)
		case v1alpha1.GameServerStateCreating:
			podPendingCount++
			handleGameServerUp(gs)
		case v1alpha1.GameServerStateStarting:
			podPendingCount++
			handleGameServerUp(gs)
		case v1alpha1.GameServerStateScheduled:
			podPendingCount++
			handleGameServerUp(gs)
		case v1alpha1.GameServerStateRequestReady:
			handleGameServerUp(gs)
		case v1alpha1.GameServerStateReady:
			handleGameServerUp(gs)
		case v1alpha1.GameServerStateShutdown:
			// will be deleted soon
			handleGameServerUp(gs)

		// GameServerStateAllocated - already handled above

		case v1alpha1.GameServerStateError, v1alpha1.GameServerStateUnhealthy:
			scheduleDeletion(gs)
		default:
			// unrecognized state, assume it's up.
			handleGameServerUp(gs)
		}
	}

	var partialReconciliation bool
	var numServersToAdd int

	if upCount < targetReplicaCount {
		numServersToAdd = targetReplicaCount - upCount
		originalNumServersToAdd := numServersToAdd

		if numServersToAdd > maxCreations {
			numServersToAdd = maxCreations
		}

		if numServersToAdd+podPendingCount > maxPending {
			numServersToAdd = maxPending - podPendingCount
			if numServersToAdd < 0 {
				numServersToAdd = 0
			}
		}

		if originalNumServersToAdd != numServersToAdd {
			partialReconciliation = true
		}
	}

	if len(toDelete) > maxDeletions {
		// we have to pick which GS to delete, let's delete the newest ones first.
		sort.Slice(toDelete, func(i, j int) bool {
			return toDelete[i].ObjectMeta.CreationTimestamp.After(toDelete[j].ObjectMeta.CreationTimestamp.Time)
		})

		toDelete = toDelete[0:maxDeletions]
		partialReconciliation = true
	}

	return numServersToAdd, toDelete, partialReconciliation
}

func isAllocated(gs *v1alpha1.GameServer) bool {
	return gs.ObjectMeta.DeletionTimestamp == nil && gs.Status.State == v1alpha1.GameServerStateAllocated
}

// addMoreGameServers adds diff more GameServers to the set
func (c *Controller) addMoreGameServers(gsSet *v1alpha1.GameServerSet, count int) error {
	c.logger.WithField("count", count).WithField("gameserverset", gsSet.ObjectMeta.Name).Info("Adding more gameservers")

	return parallelize(newGameServersChannel(count, gsSet), maxCreationParalellism, func(gs *v1alpha1.GameServer) error {
		gs, err := c.gameServerGetter.GameServers(gs.Namespace).Create(gs)
		if err != nil {
			return errors.Wrapf(err, "error creating gameserver for gameserverset %s", gsSet.ObjectMeta.Name)
		}

		c.stateCache.forGameServerSet(gsSet).created(gs)
		c.recorder.Eventf(gsSet, corev1.EventTypeNormal, "SuccessfulCreate", "Created gameserver: %s", gs.ObjectMeta.Name)
		return nil
	})
}

func (c *Controller) deleteGameServers(gsSet *v1alpha1.GameServerSet, toDelete []*v1alpha1.GameServer) error {
	c.logger.WithField("diff", len(toDelete)).WithField("gameserverset", gsSet.ObjectMeta.Name).Info("Deleting gameservers")

	return parallelize(gameServerListToChannel(toDelete), maxDeletionParallelism, func(gs *v1alpha1.GameServer) error {
		// We should not delete the gameservers directly buy set their state to shutdown and let the gameserver controller to delete
		gsCopy := gs.DeepCopy()
		gsCopy.Status.State = v1alpha1.GameServerStateShutdown
		_, err := c.gameServerGetter.GameServers(gs.Namespace).Update(gsCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating gameserver %s from status %s to Shutdown status.", gs.ObjectMeta.Name, gs.Status.State)
		}

		c.stateCache.forGameServerSet(gsSet).deleted(gs)
		c.recorder.Eventf(gsSet, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted gameserver: %s", gs.ObjectMeta.Name)
		return nil
	})
}

func newGameServersChannel(n int, gsSet *v1alpha1.GameServerSet) chan *v1alpha1.GameServer {
	gameServers := make(chan *v1alpha1.GameServer)
	go func() {
		defer close(gameServers)

		for i := 0; i < n; i++ {
			gameServers <- gsSet.GameServer()
		}
	}()

	return gameServers
}

func gameServerListToChannel(list []*v1alpha1.GameServer) chan *v1alpha1.GameServer {
	gameServers := make(chan *v1alpha1.GameServer)
	go func() {
		defer close(gameServers)

		for _, gs := range list {
			gameServers <- gs
		}
	}()

	return gameServers
}

// parallelize processes a channel of game server objects, invoking the provided callback for items in the channel with the specified degree of parallelism up to a limit.
// Returns nil if all callbacks returned nil or one of the error responses, not necessarily the first one.
func parallelize(gameServers chan *v1alpha1.GameServer, parallelism int, work func(gs *v1alpha1.GameServer) error) error {
	errch := make(chan error, parallelism)

	var wg sync.WaitGroup

	for i := 0; i < parallelism; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			for it := range gameServers {
				err := work(it)
				if err != nil {
					errch <- err
					break
				}
			}
		}()
	}
	wg.Wait()
	close(errch)

	for range gameServers {
		// drain any remaining game servers in the channel, in case we did not consume them all
	}

	// return first error from the channel, or nil if all successful.
	return <-errch
}

// syncGameServerSetStatus synchronises the GameServerSet State with active GameServer counts
func (c *Controller) syncGameServerSetStatus(gsSet *v1alpha1.GameServerSet, list []*v1alpha1.GameServer) error {
	return c.updateStatusIfChanged(gsSet, computeStatus(list))
}

// updateStatusIfChanged updates GameServerSet status if it's different than provided.
func (c *Controller) updateStatusIfChanged(gsSet *v1alpha1.GameServerSet, status v1alpha1.GameServerSetStatus) error {
	if gsSet.Status != status {
		gsSetCopy := gsSet.DeepCopy()
		gsSetCopy.Status = status
		_, err := c.gameServerSetGetter.GameServerSets(gsSet.ObjectMeta.Namespace).Update(gsSetCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating status on GameServerSet %s", gsSet.ObjectMeta.Name)
		}
	}
	return nil
}

// computeStatus computes the status of the game server set.
func computeStatus(list []*v1alpha1.GameServer) v1alpha1.GameServerSetStatus {
	var status v1alpha1.GameServerSetStatus

	status.Replicas = int32(len(list))
	for _, gs := range list {
		switch gs.Status.State {
		case v1alpha1.GameServerStateReady:
			status.ReadyReplicas++
		case v1alpha1.GameServerStateAllocated:
			status.AllocatedReplicas++
		}
	}

	return status
}
