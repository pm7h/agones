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
	"sort"
	"sync"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/runtime"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// A set of port allocations for a node
type portAllocation map[int32]bool

// PortAllocator manages the dynamic port
// allocation strategy. Only use exposed methods to ensure
// appropriate locking is taken.
// The PortAllocator does not currently support mixing static portAllocations (or any pods with defined HostPort)
// within the dynamic port range other than the ones it coordinates.
type PortAllocator struct {
	logger             *logrus.Entry
	mutex              sync.RWMutex
	portAllocations    []portAllocation
	gameServerRegistry map[types.UID]bool
	minPort            int32
	maxPort            int32
	gameServerSynced   cache.InformerSynced
	gameServerLister   listerv1alpha1.GameServerLister
	gameServerInformer cache.SharedIndexInformer
	nodeSynced         cache.InformerSynced
	nodeLister         corelisterv1.NodeLister
	nodeInformer       cache.SharedIndexInformer
}

// NewPortAllocator returns a new dynamic port
// allocator. minPort and maxPort are the top and bottom portAllocations that can be allocated in the range for
// the game servers
func NewPortAllocator(minPort, maxPort int32,
	kubeInformerFactory informers.SharedInformerFactory,
	agonesInformerFactory externalversions.SharedInformerFactory) *PortAllocator {

	v1 := kubeInformerFactory.Core().V1()
	nodes := v1.Nodes()
	gameServers := agonesInformerFactory.Stable().V1alpha1().GameServers()

	pa := &PortAllocator{
		mutex:              sync.RWMutex{},
		minPort:            minPort,
		maxPort:            maxPort,
		gameServerRegistry: map[types.UID]bool{},
		gameServerSynced:   gameServers.Informer().HasSynced,
		gameServerLister:   gameServers.Lister(),
		gameServerInformer: gameServers.Informer(),
		nodeLister:         nodes.Lister(),
		nodeInformer:       nodes.Informer(),
		nodeSynced:         nodes.Informer().HasSynced,
	}
	pa.logger = runtime.NewLoggerWithType(pa)

	pa.gameServerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: pa.syncDeleteGameServer,
	})

	pa.logger.WithField("minPort", minPort).WithField("maxPort", maxPort).Info("Starting")
	return pa
}

// Run sets up the current state of port allocations and
// starts tracking Pod and Node changes
func (pa *PortAllocator) Run(stop <-chan struct{}) error {
	pa.logger.Info("Running")

	if !cache.WaitForCacheSync(stop, pa.gameServerSynced, pa.nodeSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	// on run, let's make sure we start with a perfect slate straight away
	if err := pa.syncAll(); err != nil {
		return errors.Wrap(err, "error performing initial sync")
	}

	return nil
}

// Allocate assigns a port to the GameServer and returns it.
// Return ErrPortNotFound if no port is allocatable
func (pa *PortAllocator) Allocate(gs *v1alpha1.GameServer) *v1alpha1.GameServer {
	pa.mutex.Lock()
	defer pa.mutex.Unlock()

	type pn struct {
		pa   portAllocation
		port int32
	}

	// we only want this to be called inside the mutex lock
	// so let's define the function here so it can never be called elsewhere.
	// Also the return gives an escape from the double loop
	findOpenPorts := func(amount int) []pn {
		var ports []pn
		for _, n := range pa.portAllocations {
			for p, taken := range n {
				if !taken {
					ports = append(ports, pn{pa: n, port: p})
					// only allocate as many ports as are asked for by the GameServer
					if len(ports) == amount {
						return ports
					}
				}
			}
		}
		return ports
	}

	// this allows us to do recursion, within the mutex lock
	var allocate func(gs *v1alpha1.GameServer) *v1alpha1.GameServer
	allocate = func(gs *v1alpha1.GameServer) *v1alpha1.GameServer {
		amount := gs.CountPorts(v1alpha1.Dynamic)
		allocations := findOpenPorts(amount)

		if len(allocations) == amount {
			pa.gameServerRegistry[gs.ObjectMeta.UID] = true

			for i, p := range gs.Spec.Ports {
				if p.PortPolicy == v1alpha1.Dynamic {
					// pop off allocation
					var a pn
					a, allocations = allocations[0], allocations[1:]
					a.pa[a.port] = true
					gs.Spec.Ports[i].HostPort = a.port
				}
			}

			return gs
		}

		// if we get here, we ran out of ports. Add a node, and try again.
		// this is important, because to autoscale scale up, we create GameServers that
		// can't be scheduled on the current set of nodes, so we need to be sure
		// there are always ports available to be allocated.
		pa.portAllocations = append(pa.portAllocations, pa.newPortAllocation())

		return allocate(gs)
	}

	return allocate(gs)
}

// DeAllocate marks the given port as no longer allocated
func (pa *PortAllocator) DeAllocate(gs *v1alpha1.GameServer) {
	// skip if it wasn't previously allocated

	found := func() bool {
		pa.mutex.RLock()
		defer pa.mutex.RUnlock()
		if _, ok := pa.gameServerRegistry[gs.ObjectMeta.UID]; ok {
			return true
		}
		return false
	}

	if !found() {
		pa.logger.WithField("gs", gs.ObjectMeta.Name).
			Info("Did not allocate this GameServer. Ignoring for DeAllocation")
		return
	}

	pa.mutex.Lock()
	defer pa.mutex.Unlock()
	for _, p := range gs.Spec.Ports {
		if p.HostPort < pa.minPort || p.HostPort > pa.maxPort {
			continue
		}
		pa.portAllocations = setPortAllocation(p.HostPort, pa.portAllocations, false)
	}

	delete(pa.gameServerRegistry, gs.ObjectMeta.UID)
}

// syncDeleteGameServer when a GameServer Pod is deleted
// make the HostPort available
func (pa *PortAllocator) syncDeleteGameServer(object interface{}) {
	if gs, ok := object.(*v1alpha1.GameServer); ok {
		pa.logger.WithField("gs", gs).Info("syncing deleted GameServer")
		pa.DeAllocate(gs)
	}
}

// syncAll syncs the pod, node and gameserver caches then
// traverses all Nodes in the cluster and all looks at GameServers
// and Terminating Pods values make sure those
// portAllocations are marked as taken.
// Locks the mutex while doing this.
// This is basically a stop the world Garbage Collection on port allocations, but it only happens on startup.
func (pa *PortAllocator) syncAll() error {
	pa.mutex.Lock()
	defer pa.mutex.Unlock()

	pa.logger.Info("Resetting Port Allocation")

	nodes, err := pa.nodeLister.List(labels.Everything())
	if err != nil {
		return errors.Wrap(err, "error listing all nodes")
	}

	gameservers, err := pa.gameServerLister.List(labels.Everything())
	if err != nil {
		return errors.Wrapf(err, "error listing all GameServers")
	}

	gsRegistry := map[types.UID]bool{}

	// place to put GameServer port allocations that are not ready yet/after the ready state
	allocations, nonReadyNodesPorts := pa.registerExistingGameServerPorts(gameservers, nodes, gsRegistry)

	// close off the port on the first node you find
	// we actually don't mind what node it is, since we only care
	// that there is a port open *somewhere* as the default scheduler
	// will re-route for us based on HostPort allocation
	for _, p := range nonReadyNodesPorts {
		allocations = setPortAllocation(p, allocations, true)
	}

	pa.portAllocations = allocations
	pa.gameServerRegistry = gsRegistry

	return nil
}

// registerExistingGameServerPorts registers the gameservers against gsRegistry and the ports against nodePorts.
// and returns an ordered list of portAllocations per cluster nodes, and an array of
// any GameServers allocated a port, but not yet assigned a Node will returned as an array of port values.
func (pa *PortAllocator) registerExistingGameServerPorts(gameservers []*v1alpha1.GameServer, nodes []*corev1.Node, gsRegistry map[types.UID]bool) ([]portAllocation, []int32) {
	// setup blank port values
	nodePortAllocation := pa.nodePortAllocation(nodes)
	nodePortCount := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		nodePortCount[n.ObjectMeta.Name] = 0
	}

	var nonReadyNodesPorts []int32

	for _, gs := range gameservers {
		for _, p := range gs.Spec.Ports {
			if p.PortPolicy == v1alpha1.Dynamic {
				gsRegistry[gs.ObjectMeta.UID] = true

				// if the node doesn't exist, it's likely unscheduled
				_, ok := nodePortAllocation[gs.Status.NodeName]
				if gs.Status.NodeName != "" && ok {
					nodePortAllocation[gs.Status.NodeName][p.HostPort] = true
					nodePortCount[gs.Status.NodeName]++
				} else if p.HostPort != 0 {
					nonReadyNodesPorts = append(nonReadyNodesPorts, p.HostPort)
				}
			}
		}
	}

	// make a list of the keys
	keys := make([]string, 0, len(nodePortAllocation))
	for k := range nodePortAllocation {
		keys = append(keys, k)
	}

	// sort, since this is how it would have originally been allocated across the
	// ordered []portAllocation
	sort.Slice(keys, func(i, j int) bool {
		return nodePortCount[keys[i]] > nodePortCount[keys[j]]
	})

	// this gives us back an ordered node list
	allocations := make([]portAllocation, len(nodePortAllocation))
	for i, k := range keys {
		allocations[i] = nodePortAllocation[k]

	}

	return allocations, nonReadyNodesPorts
}

// nodePortAllocation returns a map of port allocations all set to being available
// with a map key for each node, as well as the node registry record (since we're already looping)
func (pa *PortAllocator) nodePortAllocation(nodes []*corev1.Node) map[string]portAllocation {
	nodePorts := map[string]portAllocation{}

	for _, n := range nodes {
		// ignore unschedulable nodes
		if !n.Spec.Unschedulable {
			nodePorts[n.Name] = pa.newPortAllocation()
		}
	}

	return nodePorts
}

func (pa *PortAllocator) newPortAllocation() portAllocation {
	p := make(portAllocation, (pa.maxPort-pa.minPort)+1)
	for i := pa.minPort; i <= pa.maxPort; i++ {
		p[i] = false
	}

	return p
}

// setPortAllocation takes a port from an all
func setPortAllocation(port int32, allocations []portAllocation, taken bool) []portAllocation {
	for _, np := range allocations {
		if np[port] != taken {
			np[port] = taken
			break
		}
	}
	return allocations
}
