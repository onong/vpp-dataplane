// Copyright (C) 2020 Cisco Systems Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package policy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	calicoapi "github.com/projectcalico/libcalico-go/lib/apis/v3"
	calicov3 "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/options"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/common"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/config"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/proto"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/routing"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/routing/connectivity"
	"github.com/projectcalico/vpp-dataplane/vpplink"
	"github.com/projectcalico/vpp-dataplane/vpplink/types"
	"github.com/prometheus/common/log"
	"github.com/sirupsen/logrus"
)

const (
	FelixPluginSrcPath = "/bin/felix-api-proxy"
	FelixPluginDstPath = "/var/lib/calico/felix-plugins/felix-api-proxy"
)

type SyncState int

const (
	StateDisconnected SyncState = iota
	StateConnected
	StateSyncing
	StateInSync
)

// Server holds all the data required to configure the policies defined by felix in VPP
type Server struct {
	*common.CalicoVppServerData
	log            *logrus.Entry
	vpp            *vpplink.VppLink
	calico         calicov3.Interface
	vppRestarted   chan bool
	felixRestarted chan bool
	exiting        chan bool
	routingServer  *routing.Server

	state         SyncState
	nextSeqNumber uint64

	endpointsLock       sync.Mutex
	endpointsInterfaces map[WorkloadEndpointID]uint32

	configuredState *PolicyState
	pendingState    *PolicyState

	failSafePolicy        *Policy
	workloadsToHostIPSet  *IPSet
	workloadsToHostPolicy *Policy
	allowFromHostPolicy   *Policy
	allowToHostPolicy     *Policy
	ip4                   *net.IP
	ip6                   *net.IP
	uplinkToTap           map[uint32]uint32
}

// NewServer creates a policy server
func NewServer(vpp *vpplink.VppLink, log *logrus.Entry, rs *routing.Server) (*Server, error) {
	calico, err := calicov3.NewFromEnv()
	if err != nil {
		panic(err.Error())
	}
	node, err := calico.Nodes().Get(context.Background(), config.NodeName, options.GetOptions{})
	if err != nil {
		panic(err.Error())
	}

	server := &Server{
		log:            log,
		vpp:            vpp,
		calico:         calico,
		vppRestarted:   make(chan bool),
		felixRestarted: make(chan bool),
		exiting:        make(chan bool),
		routingServer:  rs,

		state:         StateDisconnected,
		nextSeqNumber: 0,

		endpointsInterfaces: make(map[WorkloadEndpointID]uint32),

		configuredState: NewPolicyState(),
		pendingState:    NewPolicyState(),
	}

	server.uplinkToTap, err = server.mapUplinkToTapInterfaces()
	if err != nil {
		return nil, errors.Wrapf(err, "error in mapping uplink to tap interfaces")
	}
	server.setNodeIPs(&node.Spec)

	// Cleanup potentially left over socket
	err = os.RemoveAll(config.FelixDataplaneSocket)
	if err != nil {
		return nil, errors.Wrapf(err, "Could not delete socket %s", config.FelixDataplaneSocket)
	}

	err = InstallFelixPlugin()
	if err != nil {
		return nil, errors.Wrap(err, "could not install felix plugin")
	}

	return server, nil
}

func (s *Server) mapUplinkToTapInterfaces() (uplinkTap map[uint32]uint32, err error) {
	uplinkTap = make(map[uint32]uint32)
	uplinkSwifindexes, err := s.vpp.SearchInterfacesWithTagPrefix("main-")
	if err != nil {
		return nil, err
	}
	tapSwifindexes, err := s.vpp.SearchInterfacesWithTagPrefix("host-")
	if err != nil {
		return nil, err
	}
	for intf, uplink := range uplinkSwifindexes {
		tap, found := tapSwifindexes["host-"+intf[5:]]
		if found {
			uplinkTap[uplink] = tap
		} else {
			return nil, errors.Errorf("uplink interface %s not corresponding to a tap interface", uplink)
		}
	}
	return uplinkTap, nil
}

func InstallFelixPlugin() (err error) {
	err = os.RemoveAll(FelixPluginDstPath)
	if err != nil {
		log.Warnf("Could not delete %s: %v", FelixPluginDstPath, err)
	}

	in, err := os.Open(FelixPluginSrcPath)
	if err != nil {
		return errors.Wrap(err, "cannot open felix plugin to copy")
	}
	defer in.Close()

	out, err := os.OpenFile(FelixPluginDstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return errors.Wrap(err, "cannot open felix plugin to write")
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = errors.Wrap(cerr, "cannot close felix plugin file")
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return errors.Wrap(err, "cannot copy data")
	}
	err = out.Sync()
	return errors.Wrapf(err, "could not sync felix plugin changes")
}

func (s *Server) setNodeIPs(nodeSpec *calicoapi.NodeSpec) {
	if nodeSpec == nil {
		return
	} else if nodeSpec.BGP == nil {
		return
	}
	if nodeSpec.BGP.IPv4Address != "" {
		addr, _, err := net.ParseCIDR(nodeSpec.BGP.IPv4Address)
		if err != nil {
			s.log.Errorf("cannot parse node address %s: %v", nodeSpec.BGP.IPv4Address, err)
		} else {
			s.ip4 = &addr
		}
	}
	if nodeSpec.BGP.IPv6Address != "" {
		addr, _, err := net.ParseCIDR(nodeSpec.BGP.IPv6Address)
		if err != nil {
			s.log.Errorf("cannot parse node address %s: %v", nodeSpec.BGP.IPv6Address, err)
		} else {
			s.ip6 = &addr
		}
	}
}

// OnVppRestart notifies the policy server that vpp restarted
func (s *Server) OnVppRestart() {
	s.log.Warnf("Signaled VPP restart to Policy server")
	s.vppRestarted <- true
}

// WorkloadAdded is called by the CNI server when a container interface is created,
// either during startup when reconnecting the interfaces, or when a new pod is created
func (s *Server) WorkloadAdded(id *WorkloadEndpointID, swIfIndex uint32, containerIPs []*net.IPNet) {
	// TODO: Send WorkloadEndpointStatusUpdate to felix
	s.endpointsLock.Lock()
	defer s.endpointsLock.Unlock()

	intf, existing := s.endpointsInterfaces[*id]

	if existing {
		if swIfIndex != intf {
			// VPP restarted and interfaces are being reconnected
			s.log.Warnf("workload endpoint changed interfaces, did VPP restart? %v %d -> %d", id, intf, swIfIndex)
			s.endpointsInterfaces[*id] = swIfIndex
		}
		return
	}

	s.log.Infof("workload endpoint added: %v -> %d", id, swIfIndex)
	s.endpointsInterfaces[*id] = swIfIndex

	if s.state == StateInSync {
		wep, ok := s.configuredState.WorkloadEndpoints[*id]
		if !ok {
			// Nothing to configure
		} else {
			err := wep.Create(s.vpp, swIfIndex, s.configuredState)
			if err != nil {
				s.log.Errorf("Error processing workload addition: %s", err)
			}
		}
	}
	// EndpointToHostAction
	if strings.ToUpper(config.EndpointToHostAction) == "DROP" {
		for _, containerIP := range containerIPs {
			s.workloadsToHostIPSet.AddMembers([]string{containerIP.IP.String()}, true, s.vpp)
		}
	}
}

// WorkloadRemoved is called by the CNI server when the interface of a pod is deleted
func (s *Server) WorkloadRemoved(id *WorkloadEndpointID, containerIPs []*net.IPNet) {
	// TODO: Send WorkloadEndpointStatusRemove to felix
	s.endpointsLock.Lock()
	defer s.endpointsLock.Unlock()

	_, existing := s.endpointsInterfaces[*id]
	if !existing {
		s.log.Errorf("nonexistent workload endpoint removed %v", id)
		return
	}
	s.log.Infof("workload endpoint removed: %v", id)

	if s.state == StateInSync {
		wep, ok := s.configuredState.WorkloadEndpoints[*id]
		if !ok {
			// Nothing to clean up
		} else {
			err := wep.Delete(s.vpp)
			if err != nil {
				s.log.Errorf("Error processing workload removal: %s", err)
			}
		}
	}
	delete(s.endpointsInterfaces, *id)
	// EndpointToHostAction
	if strings.ToUpper(config.EndpointToHostAction) == "DROP" {
		for _, containerIP := range containerIPs {
			s.workloadsToHostIPSet.RemoveMembers([]string{containerIP.IP.String()}, true, s.vpp)
		}
	}
}

// Serve runs the policy server
func (s *Server) Serve() {
	s.log.Info("Starting policy server")

	if !config.EnablePolicies {
		s.log.Warn("Policies disabled, policy server will not configure VPP")
	}

	listener, err := net.Listen("unix", config.FelixDataplaneSocket)
	if err != nil {
		s.log.WithError(err).Errorf("Could not bind to unix://%s", config.FelixDataplaneSocket)
		return
	}
	defer func() {
		listener.Close()
		os.RemoveAll(config.FelixDataplaneSocket)
	}()

	s.createAllowFromHostPolicy()
	s.createEndpointToHostPolicy()
	s.createAllowToHostPolicy()

	for {
		s.state = StateDisconnected
		// Accept only one connection
		conn, err := listener.Accept()
		if err != nil {
			s.log.WithError(err).Error("cannot accept policy client connection")
			return
		}
		s.log.Infof("Accepted connection from felix")
		s.state = StateConnected

		go s.SyncPolicy(conn)

		select {
		case <-s.vppRestarted:
			// Close connection to restart felix, wipe all data and start over
			s.log.Infof("VPP restarted, triggering Felix restart")
			s.configuredState = NewPolicyState()
			s.endpointsInterfaces = make(map[WorkloadEndpointID]uint32)
			// This should stop the SyncPolicy goroutine and trigger a write on the felixRestarted channel
			err = conn.Close()
			if err != nil {
				s.log.WithError(err).Warn("Error closing unix connection to felix API proxy")
			}
			s.log.Infof("Waiting for SyncPolicy to stop...")
			<-s.felixRestarted
			s.log.Infof("SyncPolicy exited, reconnecting to felix")
			s.createAllowFromHostPolicy()
			s.createEndpointToHostPolicy()
			s.createAllowToHostPolicy()
		case <-s.felixRestarted:
			s.log.Infof("Felix restarted, starting resync")
			// Connection was closed. Just accept new one, state will be reconciled on startup.
		case <-s.exiting:
			s.log.Infof("Policy server exiting")
			err = conn.Close()
			if err != nil {
				s.log.WithError(err).Warn("Error closing unix connection to felix API proxy")
			}
			s.log.Infof("Waiting for SyncPolicy to stop...")
			<-s.felixRestarted
			return
		}
	}
}

func (s *Server) RescanState() error {
	return nil
}

// Stop tells the policy server to exit
func (s *Server) Stop() {
	s.exiting <- true
}

// SyncPolicy does the bulk of the policy sync job. It starts by reconciling the current
// configured state in VPP (empty at first) with what is sent by felix, and once both are in
// sync, it keeps processing felix updates. It also sends endpoint updates to felix when the
// CNI component adds or deletes container interfaces.
func (s *Server) SyncPolicy(conn net.Conn) {
	s.log.Info("Starting policy resync")
	felixUpdates := s.MessageReader(conn)
	for {
		var err error
		select {
		case msg, ok := <-felixUpdates:
			if !ok {
				s.log.Errorf("Error getting felix update: %v %v", msg, ok)
				conn.Close()
				s.felixRestarted <- true
				return
			}
			s.log.Debugf("Got message from felix: %+v", msg)
			switch m := msg.(type) {
			case *proto.ConfigUpdate:
				err = s.handleConfigUpdate(m)
			case *proto.InSync:
				err = s.handleInSync(m)
			default:
				if !config.EnablePolicies {
					// Skip processing of policy messages
					continue
				}
				var pending bool
				if s.state == StateSyncing {
					pending = true
				} else if s.state == StateInSync {
					pending = false
				} else {
					s.log.Errorf("Got message %+v but not in syncing or synced state", m)
					conn.Close()
					s.felixRestarted <- true
					return
				}
				switch m := msg.(type) {
				case *proto.IPSetUpdate:
					err = s.handleIpsetUpdate(m, pending)
				case *proto.IPSetDeltaUpdate:
					err = s.handleIpsetDeltaUpdate(m, pending)
				case *proto.IPSetRemove:
					err = s.handleIpsetRemove(m, pending)
				case *proto.ActivePolicyUpdate:
					err = s.handleActivePolicyUpdate(m, pending)
				case *proto.ActivePolicyRemove:
					err = s.handleActivePolicyRemove(m, pending)
				case *proto.ActiveProfileUpdate:
					err = s.handleActiveProfileUpdate(m, pending)
				case *proto.ActiveProfileRemove:
					err = s.handleActiveProfileRemove(m, pending)
				case *proto.HostEndpointUpdate:
					err = s.handleHostEndpointUpdate(m, pending)
				case *proto.HostEndpointRemove:
					err = s.handleHostEndpointRemove(m, pending)
				case *proto.WorkloadEndpointUpdate:
					err = s.handleWorkloadEndpointUpdate(m, pending)
				case *proto.WorkloadEndpointRemove:
					err = s.handleWorkloadEndpointRemove(m, pending)
				case *proto.HostMetadataUpdate:
					err = s.handleHostMetadataUpdate(m, pending)
				case *proto.HostMetadataRemove:
					err = s.handleHostMetadataRemove(m, pending)
				case *proto.IPAMPoolUpdate:
					err = s.handleIpamPoolUpdate(m, pending)
				case *proto.IPAMPoolRemove:
					err = s.handleIpamPoolRemove(m, pending)
				case *proto.ServiceAccountUpdate:
					err = s.handleServiceAccountUpdate(m, pending)
				case *proto.ServiceAccountRemove:
					err = s.handleServiceAccountRemove(m, pending)
				case *proto.NamespaceUpdate:
					err = s.handleNamespaceUpdate(m, pending)
				case *proto.NamespaceRemove:
					err = s.handleNamespaceRemove(m, pending)
				default:
					s.log.Warnf("Unhandled message from felix: %v", m)
				}
			}
			if err != nil {
				s.log.WithError(err).Error("Error processing update from felix, restarting")
				conn.Close()
				s.felixRestarted <- true
				// TODO: Restart VPP as well? State is left over there...
				return
			}
		case tunnelChange := <-s.routingServer.ConnectivityServer.GetTunnelChangeChan():
			var pending bool
			if s.state == StateSyncing {
				pending = true
			} else if s.state == StateInSync {
				pending = false
			} else {
				s.log.Errorf("Got tunnel change %+v but not in syncing or synced state", tunnelChange)
				continue
			}
			state := s.currentState(pending)
			for _, h := range state.HostEndpoints {
				if tunnelChange.ChangeType == connectivity.AddChange {
					newTunnel := true
					for _, v := range h.TunnelSwIfIndexes {
						if v == tunnelChange.Swifindex {
							newTunnel = false
						}
					}
					if newTunnel {
						h.TunnelSwIfIndexes = append(h.TunnelSwIfIndexes, tunnelChange.Swifindex)
						s.log.Infof("Configuring policies on added tunnel [%d]", tunnelChange.Swifindex)
						err = s.vpp.ConfigurePolicies(tunnelChange.Swifindex, h.currentForwardConf)
						if err != nil {
							s.log.Errorf("cannot configure policies on tunnel interface %d", tunnelChange.Swifindex)
						}
					}
				} else { // delete case
					s.log.Infof("Tunnel [%d] is deleted", tunnelChange.Swifindex)
					for index, existingSwifindex := range h.TunnelSwIfIndexes {
						if existingSwifindex == tunnelChange.Swifindex {
							h.TunnelSwIfIndexes = append(h.TunnelSwIfIndexes[:index], h.TunnelSwIfIndexes[index+1:]...)
						}
					}
				}
			}
		}
	}
}

func (s *Server) currentState(pending bool) *PolicyState {
	if pending {
		return s.pendingState
	}
	return s.configuredState
}

func (s *Server) handleConfigUpdate(msg *proto.ConfigUpdate) (err error) {
	if s.state != StateConnected {
		return fmt.Errorf("Received ConfigUpdate but server is not in Connected state! state: %v", s.state)
	}
	s.log.Infof("Got config from felix: %+v", msg)
	s.state = StateSyncing

	config.HandleFelixConfig(msg.Config)
	err = s.createFailSafePolicies(config.FailsafeInboundHostPorts, config.FailsafeOutboundHostPorts)
	if err != nil {
		return err
	}
	return nil
}

func (s *Server) handleInSync(msg *proto.InSync) (err error) {
	if s.state != StateSyncing {
		return fmt.Errorf("Received InSync but state was not syncing")
	}
	s.endpointsLock.Lock()
	defer s.endpointsLock.Unlock()

	s.state = StateInSync
	s.log.Infof("Policies now in sync")
	return s.applyPendingState()
}

func (s *Server) handleIpsetUpdate(msg *proto.IPSetUpdate, pending bool) (err error) {
	ips, err := fromIPSetUpdate(msg)
	if err != nil {
		return errors.Wrap(err, "cannot process IPSetUpdate")
	}
	state := s.currentState(pending)
	_, ok := state.IPSets[msg.GetId()]
	if ok {
		return fmt.Errorf("Received new ipset for ID %s that already exists", msg.GetId())
	}
	if !pending {
		err = ips.Create(s.vpp)
		if err != nil {
			return errors.Wrapf(err, "cannot create ipset %s", msg.GetId())
		}
	}
	state.IPSets[msg.GetId()] = ips
	return nil
}

func (s *Server) handleIpsetDeltaUpdate(msg *proto.IPSetDeltaUpdate, pending bool) (err error) {
	ips, ok := s.currentState(pending).IPSets[msg.GetId()]
	if !ok {
		return fmt.Errorf("received delta update for non-existent ipset")
	}
	err = ips.AddMembers(msg.GetAddedMembers(), !pending, s.vpp)
	if err != nil {
		return errors.Wrap(err, "cannot process ipset delta update")
	}
	err = ips.RemoveMembers(msg.GetRemovedMembers(), !pending, s.vpp)
	if err != nil {
		return errors.Wrap(err, "cannot process ipset delta update")
	}
	return nil
}

func (s *Server) handleIpsetRemove(msg *proto.IPSetRemove, pending bool) (err error) {
	state := s.currentState(pending)
	_, ok := state.IPSets[msg.GetId()]
	if !ok {
		s.log.Debugf("Received ipset delete for ID %s that doesn't exists", msg.GetId())
		return nil
	}
	if !pending {
		err = state.IPSets[msg.GetId()].Delete(s.vpp)
		if err != nil {
			return errors.Wrapf(err, "cannot delete ipset %s", msg.GetId())
		}
	}
	delete(state.IPSets, msg.GetId())
	return nil
}

func (s *Server) handleActivePolicyUpdate(msg *proto.ActivePolicyUpdate, pending bool) (err error) {
	state := s.currentState(pending)
	id := PolicyID{
		Tier: msg.Id.Tier,
		Name: msg.Id.Name,
	}
	p, err := fromProtoPolicy(msg.Policy)
	if err != nil {
		return errors.Wrapf(err, "cannot process policy update")
	}

	existing, ok := state.Policies[id]
	if ok { // Policy with this ID already exists
		if pending {
			// Just replace policy in pending state
			state.Policies[id] = p
		} else {
			return errors.Wrap(existing.Update(s.vpp, p, state), "cannot update policy")
		}
	} else {
		// Create it in state
		state.Policies[id] = p
		if !pending {
			return errors.Wrap(p.Create(s.vpp, state), "cannot create policy")
		}
	}
	return nil
}

func (s *Server) handleActivePolicyRemove(msg *proto.ActivePolicyRemove, pending bool) (err error) {
	state := s.currentState(pending)
	id := PolicyID{
		Tier: msg.Id.Tier,
		Name: msg.Id.Name,
	}
	existing, ok := state.Policies[id]
	if !ok {
		s.log.Debugf("Received policy delete for Tier %s Name %s that doesn't exists", id.Tier, id.Name)
		return nil
	}
	if !pending {
		err = existing.Delete(s.vpp, state)
		if err != nil {
			return errors.Wrap(err, "error deleting policy")
		}
	}
	delete(state.Policies, id)
	return nil
}

func (s *Server) handleActiveProfileUpdate(msg *proto.ActiveProfileUpdate, pending bool) (err error) {
	state := s.currentState(pending)
	id := msg.Id.Name
	p, err := fromProtoProfile(msg.Profile)
	if err != nil {
		return errors.Wrapf(err, "cannot process profile update")
	}

	existing, ok := state.Profiles[id]
	if ok { // Policy with this ID already exists
		if pending {
			// Just replace policy in pending state
			state.Profiles[id] = p
		} else {
			return errors.Wrap(existing.Update(s.vpp, p, state), "cannot update profile")
		}
	} else {
		// Create it in state
		state.Profiles[id] = p
		if !pending {
			return errors.Wrap(p.Create(s.vpp, state), "cannot create profile")
		}
	}
	return nil
}

func (s *Server) handleActiveProfileRemove(msg *proto.ActiveProfileRemove, pending bool) (err error) {
	state := s.currentState(pending)
	id := msg.Id.Name
	existing, ok := state.Profiles[id]
	if !ok {
		s.log.Debugf("Received profile delete for Name %s that doesn't exists", id)
		return nil
	}
	if !pending {
		err = existing.Delete(s.vpp, state)
		if err != nil {
			return errors.Wrap(err, "error deleting profile")
		}
	}
	delete(state.Profiles, id)
	return nil
}

func (s *Server) handleHostEndpointUpdate(msg *proto.HostEndpointUpdate, pending bool) (err error) {
	state := s.currentState(pending)
	id := fromProtoHostEndpointID(msg.Id)
	hep := fromProtoHostEndpoint(msg.Endpoint, s)
	if hep.InterfaceName != "" && hep.InterfaceName != "*" {
		swifindex, err := s.vpp.SearchInterfaceWithTag("main-" + hep.InterfaceName)
		if err != nil {
			return err
		} else if swifindex == types.InvalidID {
			s.log.Errorf("cannot find host endpoint: interface named %s does not exist", hep.InterfaceName)
		} else {
			hep.UplinkSwIfIndexes = append(hep.UplinkSwIfIndexes, swifindex)
		}
		hep.TapSwIfIndexes = append(hep.TapSwIfIndexes, s.uplinkToTap[swifindex])
	} else if hep.InterfaceName == "" && hep.expectedIPs != nil {
		hep.UplinkSwIfIndexes, err = s.vpp.SearchInterfacesWithAddresses(hep.expectedIPs, "main-")
		if err != nil {
			return err
		}
		for _, swifindex := range hep.UplinkSwIfIndexes {
			hep.TapSwIfIndexes = append(hep.TapSwIfIndexes, s.uplinkToTap[swifindex])
		}
	} else if hep.InterfaceName == "*" {
		for uplink, tap := range s.uplinkToTap {
			hep.UplinkSwIfIndexes = append(hep.UplinkSwIfIndexes, uplink)
			hep.TapSwIfIndexes = append(hep.TapSwIfIndexes, tap)
		}
	}
	for _, provider := range s.routingServer.ConnectivityServer.Providers {
		hep.TunnelSwIfIndexes = append(hep.TunnelSwIfIndexes, provider.GetSwifindexes()...)
	}
	if len(hep.UplinkSwIfIndexes) == 0 || len(hep.TapSwIfIndexes) == 0 {
		return errors.Errorf("No interface to configure as host endpoint")
	}
	existing, found := state.HostEndpoints[*id]

	s.log.Infof("Updating host endpoint %s (found:%t)", id.EndpointID, found)
	if existing != nil {
		s.log.Infof("Existing: %s", existing.String())
	}
	s.log.Infof("New: %s", hep.String())

	if found {
		if pending {
			state.HostEndpoints[*id] = hep
		} else {
			return errors.Wrap(existing.Update(s.vpp, hep, state), "cannot update host endpoint")
		}
	} else {
		state.HostEndpoints[*id] = hep
		if !pending {
			return errors.Wrap(hep.Create(s.vpp, state),
				"cannot create host endpoint")
		}
	}
	return nil
}

func (s *Server) handleHostEndpointRemove(msg *proto.HostEndpointRemove, pending bool) (err error) {
	state := s.currentState(pending)
	id := fromProtoHostEndpointID(msg.Id)
	existing, ok := state.HostEndpoints[*id]
	if !ok {
		s.log.Debugf("Received host endpoint delete for %v that doesn't exists", id)
		return nil
	}
	if !pending && len(existing.UplinkSwIfIndexes) != 0 {
		s.log.Infof("Removing host endpoint")
		err = existing.Delete(s.vpp)
		if err != nil {
			return errors.Wrap(err, "error deleting host endpoint")
		}
	}
	delete(state.HostEndpoints, *id)
	return nil
}

func (s *Server) handleWorkloadEndpointUpdate(msg *proto.WorkloadEndpointUpdate, pending bool) (err error) {
	s.endpointsLock.Lock()
	defer s.endpointsLock.Unlock()

	state := s.currentState(pending)
	id := fromProtoEndpointID(msg.Id)
	wep := fromProtoWorkload(msg.Endpoint, s)

	existing, found := state.WorkloadEndpoints[*id]
	intf, intfFound := s.endpointsInterfaces[*id]

	s.log.Infof("Updating endpoint %s(found:%t) if[%d](found:%t)", id.String(), found, intf, intfFound)
	if existing != nil {
		s.log.Infof("Existing: %s", existing.String())
	}
	s.log.Infof("New: %s", wep.String())

	if found {
		if pending || !intfFound {
			state.WorkloadEndpoints[*id] = wep
		} else {
			return errors.Wrap(existing.Update(s.vpp, wep, state), "cannot update workload endpoint")
		}
	} else {
		state.WorkloadEndpoints[*id] = wep
		if !pending && intfFound {
			return errors.Wrap(wep.Create(s.vpp, intf, state), "cannot create workload endpoint")
		}
	}
	return nil
}

func (s *Server) handleWorkloadEndpointRemove(msg *proto.WorkloadEndpointRemove, pending bool) (err error) {
	s.endpointsLock.Lock()
	defer s.endpointsLock.Unlock()

	state := s.currentState(pending)
	id := fromProtoEndpointID(msg.Id)
	existing, ok := state.WorkloadEndpoints[*id]
	if !ok {
		s.log.Debugf("Received workload endpoint delete for %v that doesn't exists", id)
		return nil
	}
	if !pending && existing.SwIfIndex != types.InvalidID {
		err = existing.Delete(s.vpp)
		if err != nil {
			return errors.Wrap(err, "error deleting workload endpoint")
		}
	}
	delete(state.WorkloadEndpoints, *id)
	return nil
}

func (s *Server) handleHostMetadataUpdate(msg *proto.HostMetadataUpdate, pending bool) (err error) {
	s.log.Infof("Ignoring HostMetadataUpdate")
	return nil
}

func (s *Server) handleHostMetadataRemove(msg *proto.HostMetadataRemove, pending bool) (err error) {
	s.log.Infof("Ignoring HostMetadataRemove")
	return nil
}

func (s *Server) handleIpamPoolUpdate(msg *proto.IPAMPoolUpdate, pending bool) (err error) {
	s.log.Infof("Ignoring IpamPoolUpdate")
	return nil
}

func (s *Server) handleIpamPoolRemove(msg *proto.IPAMPoolRemove, pending bool) (err error) {
	s.log.Infof("Ignoring IpamPoolRemove")
	return nil
}

func (s *Server) handleServiceAccountUpdate(msg *proto.ServiceAccountUpdate, pending bool) (err error) {
	s.log.Infof("Ignoring ServiceAccountUpdate")
	return nil
}

func (s *Server) handleServiceAccountRemove(msg *proto.ServiceAccountRemove, pending bool) (err error) {
	s.log.Infof("Ignoring ServiceAccountRemove")
	return nil
}

func (s *Server) handleNamespaceUpdate(msg *proto.NamespaceUpdate, pending bool) (err error) {
	s.log.Infof("Ignoring NamespaceUpdate")
	return nil
}

func (s *Server) handleNamespaceRemove(msg *proto.NamespaceRemove, pending bool) (err error) {
	s.log.Infof("Ignoring NamespaceRemove")
	return nil
}

// Reconciles the pending state with the configured state
func (s *Server) applyPendingState() (err error) {
	s.log.Infof("Reconciliating pending policy state with configured state")
	// Stupid algorithm for now, delete all that is in configured state, and then recreate everything
	for _, wep := range s.configuredState.WorkloadEndpoints {
		if wep.SwIfIndex != types.InvalidID {
			err = wep.Delete(s.vpp)
			if err != nil {
				return errors.Wrap(err, "cannot cleanup workload endpoint")
			}
		}
	}
	for _, policy := range s.configuredState.Policies {
		err = policy.Delete(s.vpp, s.configuredState)
		if err != nil {
			s.log.Warnf("error deleting policy: %v", err)
		}
	}
	for _, profile := range s.configuredState.Profiles {
		err = profile.Delete(s.vpp, s.configuredState)
		if err != nil {
			s.log.Warnf("error deleting profile: %v", err)
		}
	}
	for _, ipset := range s.configuredState.IPSets {
		err = ipset.Delete(s.vpp)
		if err != nil {
			s.log.Warnf("error deleting ipset: %v", err)
		}
	}
	for _, hep := range s.configuredState.HostEndpoints {
		if len(hep.UplinkSwIfIndexes) != 0 {
			err = hep.Delete(s.vpp)
			if err != nil {
				s.log.Warnf("error deleting hostendpoint")
			}
		}
	}

	s.configuredState = s.pendingState
	s.pendingState = NewPolicyState()
	for _, ipset := range s.configuredState.IPSets {
		err = ipset.Create(s.vpp)
		if err != nil {
			return errors.Wrap(err, "error creating ipset")
		}
	}
	for _, profile := range s.configuredState.Profiles {
		err = profile.Create(s.vpp, s.configuredState)
		if err != nil {
			return errors.Wrap(err, "error creating profile")
		}
	}
	for _, policy := range s.configuredState.Policies {
		err = policy.Create(s.vpp, s.configuredState)
		if err != nil {
			return errors.Wrap(err, "error creating policy")
		}
	}
	for id, wep := range s.configuredState.WorkloadEndpoints {
		intf, intfFound := s.endpointsInterfaces[id]
		if intfFound {
			err = wep.Create(s.vpp, intf, s.configuredState)
			if err != nil {
				return errors.Wrap(err, "cannot configure workload endpoint")
			}
		}
	}
	for _, hep := range s.configuredState.HostEndpoints {
		err = hep.Create(s.vpp, s.configuredState)
		if err != nil {
			return errors.Wrap(err, "cannot create host endpoint")
		}
	}
	s.log.Infof("Reconciliation done")
	return nil
}

func (s *Server) createAllowToHostPolicy() (err error) {
	s.log.Infof("Creating policy to allow traffic to host that is applied on uplink")
	r_in := &Rule{
		VppID:  types.InvalidID,
		RuleID: "calicovpp-internal-allowtohost",
		Rule: &types.Rule{
			Action: types.ActionAllow,
			DstNet: []net.IPNet{},
		},
	}
	r_out := &Rule{
		VppID:  types.InvalidID,
		RuleID: "calicovpp-internal-allowtohost",
		Rule: &types.Rule{
			Action: types.ActionAllow,
			SrcNet: []net.IPNet{},
		},
	}
	if s.ip4 != nil {
		r_in.Rule.DstNet = append(r_in.Rule.DstNet, *common.FullyQualified(*s.ip4))
		r_out.Rule.SrcNet = append(r_out.Rule.SrcNet, *common.FullyQualified(*s.ip4))
	}
	if s.ip6 != nil {
		r_in.Rule.DstNet = append(r_in.Rule.DstNet, *common.FullyQualified(*s.ip6))
		r_out.Rule.SrcNet = append(r_out.Rule.SrcNet, *common.FullyQualified(*s.ip6))
	}

	s.allowToHostPolicy = &Policy{
		Policy: &types.Policy{},
		VppID:  types.InvalidID,
	}
	s.allowToHostPolicy.InboundRules = append(s.allowToHostPolicy.InboundRules, r_out)
	s.allowToHostPolicy.OutboundRules = append(s.allowToHostPolicy.OutboundRules, r_in)
	err = s.allowToHostPolicy.Create(s.vpp, nil)
	return errors.Wrap(err, "cannot create policy to allow traffic to host")
}

func (s *Server) createAllowFromHostPolicy() (err error) {
	s.log.Infof("Creating policy to allow traffic from host with ingress policies")
	r := &Rule{
		VppID:  types.InvalidID,
		RuleID: "calicovpp-internal-allowfromhost",
		Rule: &types.Rule{
			Action: types.ActionAllow,
			SrcNet: []net.IPNet{},
		},
	}
	if s.ip4 != nil {
		r.Rule.SrcNet = append(r.Rule.SrcNet, *common.FullyQualified(*s.ip4))
	}
	if s.ip6 != nil {
		r.Rule.SrcNet = append(r.Rule.SrcNet, *common.FullyQualified(*s.ip6))
	}

	s.allowFromHostPolicy = &Policy{
		Policy: &types.Policy{},
		VppID:  types.InvalidID,
	}
	s.allowFromHostPolicy.InboundRules = append(s.allowFromHostPolicy.InboundRules, r)
	err = s.allowFromHostPolicy.Create(s.vpp, nil)
	return errors.Wrap(err, "cannot create policy to allow traffic from host")
}

func (s *Server) createEndpointToHostPolicy( /*may be return*/ ) (err error) {
	pol := &Policy{
		Policy: &types.Policy{},
		VppID:  types.InvalidID,
	}
	r_deny_workloads := &Rule{
		VppID: types.InvalidID,
		Rule: &types.Rule{
			Action: types.ActionDeny,
		},
		SrcIPSetNames: []string{"ipset1"},
	}
	ipset := &IPSet{Type: types.IpsetTypeIP}
	ps := PolicyState{IPSets: map[string]*IPSet{"ipset1": ipset}}
	ipset.Create(s.vpp)
	pol.InboundRules = append(pol.InboundRules, r_deny_workloads)
	err = pol.Create(s.vpp, &ps)
	if err != nil {
		return err
	}
	s.workloadsToHostIPSet = ipset
	s.workloadsToHostPolicy = pol

	r := &Rule{
		VppID: types.InvalidID,
		Rule: &types.Rule{
			Action: types.ActionAllow,
		},
	}
	pol = &Policy{
		Policy: &types.Policy{},
		VppID:  types.InvalidID,
	}
	pol.InboundRules = append(pol.InboundRules, r)
	err = pol.Create(s.vpp, &ps)
	if err != nil {
		return err
	}
	conf := types.NewInterfaceConfig()
	conf.IngressPolicyIDs = append(conf.IngressPolicyIDs, s.workloadsToHostPolicy.VppID)
	conf.IngressPolicyIDs = append(conf.IngressPolicyIDs, pol.VppID)
	swifindexes, err := s.vpp.SearchInterfacesWithTagPrefix("host-") // tap interfaces
	if err != nil {
		s.log.Error(err)
	}
	for _, swifindex := range swifindexes {
		err = s.vpp.ConfigurePolicies(uint32(swifindex), conf)
		if err != nil {
			s.log.Error("cannot create policy to drop traffic to host")
		}
	}
	return nil
}

func (s *Server) createFailSafePolicies(failSafeInbound string, failSafeOutbound string) (err error) {
	failSafePol := &Policy{
		Policy: &types.Policy{},
		VppID:  types.InvalidID,
	}
	failSafeInboundRules, err := getfailSafeRules(failSafeInbound)
	if err != nil {
		return err
	}
	failSafeOutboundRules, err := getfailSafeRules(failSafeOutbound)
	if err != nil {
		return err
	}
	failSafePol.InboundRules = failSafeInboundRules
	failSafePol.OutboundRules = failSafeOutboundRules
	err = failSafePol.Create(s.vpp, nil)
	if err != nil {
		return err
	}
	s.failSafePolicy = failSafePol
	return nil
}

func getProtocolRules(protocolName string, failSafe string) (*Rule, error) {
	portRanges := []types.PortRange{}

	failSafe = strings.Join(strings.Fields(failSafe), "")
	protocolsPorts := strings.Split(failSafe, ",")
	for _, protocolPort := range protocolsPorts {
		protocolAndPort := strings.Split(protocolPort, ":")
		if len(protocolAndPort) != 2 {
			return nil, errors.Errorf("failsafe has wrong format")
		}
		protocol := protocolAndPort[0]
		port := protocolAndPort[1]
		if protocol == protocolName {
			port, err := strconv.Atoi(port)
			if err != nil {
				return nil, errors.Errorf("failsafe has wrong format")
			}
			portRanges = append(portRanges, types.PortRange{First: uint16(port), Last: uint16(port)})
		}
	}
	protocol, err := parseProtocol(&proto.Protocol{NumberOrName: &proto.Protocol_Name{Name: protocolName}})
	if err != nil {
		return nil, err
	}
	r_failsafe := &Rule{
		VppID:  types.InvalidID,
		RuleID: "failsafe" + protocolName,
		Rule: &types.Rule{
			Action:       types.ActionAllow,
			DstPortRange: portRanges,
			Filters: []types.RuleFilter{{
				ShouldMatch: true,
				Type:        types.CapoFilterProto,
				Value:       int(protocol),
			}},
		},
	}
	return r_failsafe, nil
}

func getfailSafeRules(failSafe string) ([]*Rule, error) {
	r_failsafe_tcp, err := getProtocolRules("tcp", failSafe)
	if err != nil {
		return nil, errors.Errorf("failsafe has wrong format")
	}
	r_failsafe_udp, err := getProtocolRules("udp", failSafe)
	if err != nil {
		return nil, errors.Errorf("failsafe has wrong format")
	}
	return []*Rule{r_failsafe_tcp, r_failsafe_udp}, nil
}
