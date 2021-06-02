// Copyright (C) 2021 Cisco Systems Inc.
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

package pod_interface

import (
	"github.com/pkg/errors"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/cni/storage"
	gcommon "github.com/projectcalico/vpp-dataplane/calico-vpp-agent/common"
	"github.com/projectcalico/vpp-dataplane/calico-vpp-agent/config"
	"github.com/projectcalico/vpp-dataplane/vpplink"
	"github.com/projectcalico/vpp-dataplane/vpplink/types"
	"github.com/sirupsen/logrus"
)

type PodInterfaceDriverData struct {
	log          *logrus.Entry
	vpp          *vpplink.VppLink
	isL3         bool
	name         string
	NDataThreads int
	IfType       storage.VppInterfaceType
}

func (i *PodInterfaceDriverData) SearchPodInterface(podSpec *storage.LocalPodSpec) (swIfIndex uint32) {
	tag := podSpec.GetInterfaceTag(i.name)
	i.log.Infof("looking for tag %s", tag)
	err, swIfIndex := i.vpp.SearchInterfaceWithTag(tag)
	if err != nil {
		i.log.Warnf("error searching interface with tag %s %s", tag, err)
		return vpplink.INVALID_SW_IF_INDEX
	} else if swIfIndex == vpplink.INVALID_SW_IF_INDEX {
		return vpplink.INVALID_SW_IF_INDEX
	}
	return swIfIndex
}

func (i *PodInterfaceDriverData) UndoPodInterfaceConfiguration(swIfIndex uint32) {
	i.log.Infof("found matching VPP tun[%d]", swIfIndex)
	err := i.vpp.InterfaceAdminDown(swIfIndex)
	if err != nil {
		i.log.Errorf("InterfaceAdminDown errored %s", err)
	}

	err = i.vpp.RemovePodInterface(swIfIndex)
	if err != nil {
		i.log.Errorf("error deregistering pod interface: %v", err)
	}
}

func (i *PodInterfaceDriverData) DoPodInterfaceConfiguration(podSpec *storage.LocalPodSpec, swIfIndex uint32) (err error) {
	if i.NDataThreads > 0 {
		for queue := 0; queue < config.TapNumRxQueues; queue++ {
			worker := (int(swIfIndex)*config.TapNumRxQueues + queue) % i.NDataThreads
			err = i.vpp.SetInterfaceRxPlacement(swIfIndex, queue, worker, false /*main*/)
			if err != nil {
				i.log.Warnf("failed to set tun[%d] queue%d worker%d (tot workers %d): %v", swIfIndex, queue, worker, i.NDataThreads, err)
			}
		}
	}

	// configure vpp side tun
	err = i.vpp.SetInterfaceVRF(swIfIndex, gcommon.PodVRFIndex)
	if err != nil {
		return errors.Wrapf(err, "error setting vpp tun %d in pod vrf", swIfIndex)
	}

	err = i.vpp.InterfaceSetUnnumbered(swIfIndex, config.DataInterfaceSwIfIndex)
	if err != nil {
		return errors.Wrapf(err, "error setting vpp if[%d] unnumbered", swIfIndex)
	}

	if !i.isL3 {
		/* L2 */
		err = i.vpp.SetPromiscOn(swIfIndex)
		if err != nil {
			return errors.Wrapf(err, "Error setting memif promisc")
		}
	}

	hasv4, hasv6 := podSpec.Hasv46()
	if hasv4 && podSpec.NeedsSnat {
		i.log.Infof("Enable tun[%d] SNAT v4", swIfIndex)
		err = i.vpp.EnableCnatSNAT(swIfIndex, false)
		if err != nil {
			return errors.Wrapf(err, "Error enabling ip4 snat")
		}
	}
	if hasv6 && podSpec.NeedsSnat {
		i.log.Infof("Enable tun[%d] SNAT v6", swIfIndex)
		err = i.vpp.EnableCnatSNAT(swIfIndex, true)
		if err != nil {
			return errors.Wrapf(err, "Error enabling ip6 snat")
		}
	}

	err = i.vpp.RegisterPodInterface(swIfIndex)
	if err != nil {
		return errors.Wrapf(err, "error registering pod interface")
	}

	err = i.vpp.CnatEnableFeatures(swIfIndex)
	if err != nil {
		return errors.Wrapf(err, "error configuring nat on pod interface")
	}

	err = i.vpp.InterfaceAdminUp(swIfIndex)
	if err != nil {
		return errors.Wrapf(err, "error setting new tun up")
	}

	err = i.vpp.SetInterfaceRxMode(swIfIndex, types.AllQueues, config.TapRxMode)
	if err != nil {
		return errors.Wrapf(err, "error SetInterfaceRxMode on tun interface")
	}
	return nil
}
