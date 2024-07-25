package nodelabeller

import (
	"runtime"

	"kubevirt.io/client-go/log"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/pkg/virt-handler/node-labeller/api"
)

const (
        domCapabilitiesFileName = "virsh_domcapabilities.xml"
)

type NodeCapabilitiesManager struct {
	hypervFeatures          supportedFeatures
	hostCapabilities        supportedFeatures
	supportedFeatures       []string
	cpuModelVendor          string
	volumePath              string
	domCapabilitiesFileName string
	capabilities            *api.Capabilities
	hostCPUModel            hostCPUModel
	SEV                     SEVConfiguration
}

func NewNodeCapabilitiesManager() *NodeCapabilitiesManager {
	return newNodeCapabilitiesManager(nodeLabellerVolumePath)
}

func newNodeCapabilitiesManager(volumePath string) *NodeCapabilitiesManager {
        return &NodeCapabilitiesManager{
                volumePath: volumePath,
		domCapabilitiesFileName: domCapabilitiesFileName, 
		hostCPUModel:            hostCPUModel{requiredFeatures: make(map[string]bool, 0)},
        }
}

func (n *NodeCapabilitiesManager) LoadAll() error {
	// host supported features is only available on AMD64 nodes.
	// This is because hypervisor-cpu-baseline virsh command doesnt work for ARM64 architecture.
	if virtconfig.IsAMD64(runtime.GOARCH) {
		err := n.loadHostSupportedFeatures()
		if err != nil {
                        log.Log.Errorf("node-labeller could not load supported features: " + err.Error())
			return err
		}
	}

	err := n.loadDomCapabilities()
	if err != nil {
		log.Log.Errorf("node-labeller could not load host dom capabilities: " + err.Error())
		return err
	}

	err = n.loadHostCapabilities()
	if err != nil {
		log.Log.Errorf("node-labeller could not load host capabilities: " + err.Error())
		return err
	}

	n.loadHypervFeatures()

	return nil
}

func (n *NodeCapabilitiesManager) loadHypervFeatures() {
	n.hypervFeatures.items = getCapLabels()
}

func (n *NodeCapabilitiesManager) HostCapabilities() *api.Capabilities {
	return n.capabilities
}

