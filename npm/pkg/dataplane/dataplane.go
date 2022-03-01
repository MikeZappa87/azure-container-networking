package dataplane

import (
	"fmt"
	"time"

	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/pkg/dataplane/ipsets"
	"github.com/Azure/azure-container-networking/npm/pkg/dataplane/policies"
	"github.com/Azure/azure-container-networking/npm/util"
	npmerrors "github.com/Azure/azure-container-networking/npm/util/errors"
	"k8s.io/klog"
)

const (
	// AzureNetworkName is default network Azure CNI creates
	AzureNetworkName = "azure"

	reconcileTimeInMinutes = 5
)

type PolicyMode string

// TODO put NodeName in Config?
type Config struct {
	*ipsets.IPSetManagerCfg
	*policies.PolicyManagerCfg
}

type DataPlane struct {
	*Config
	policyMgr *policies.PolicyManager
	ipsetMgr  *ipsets.IPSetManager
	networkID string
	nodeName  string
	// Key is PodIP
	endpointCache  map[string]*NPMEndpoint
	ioShim         *common.IOShim
	updatePodCache map[string]*updateNPMPod
	stopChannel    <-chan struct{}
}

type NPMEndpoint struct {
	Name string
	ID   string
	IP   string
	// TODO: check it may use PolicyKey instead of Policy name
	// Map with Key as Network Policy name to to emulate set
	// and value as struct{} for minimal memory consumption
	NetPolReference map[string]struct{}
}

func NewDataPlane(nodeName string, ioShim *common.IOShim, cfg *Config, stopChannel <-chan struct{}) (*DataPlane, error) {
	metrics.InitializeAll()
	dp := &DataPlane{
		Config:         cfg,
		policyMgr:      policies.NewPolicyManager(ioShim, cfg.PolicyManagerCfg),
		ipsetMgr:       ipsets.NewIPSetManager(cfg.IPSetManagerCfg, ioShim),
		endpointCache:  make(map[string]*NPMEndpoint),
		nodeName:       nodeName,
		ioShim:         ioShim,
		updatePodCache: make(map[string]*updateNPMPod),
		stopChannel:    stopChannel,
	}

	err := dp.BootupDataplane()
	if err != nil {
		klog.Errorf("Failed to reset dataplane: %v", err)
		return nil, err
	}
	return dp, nil
}

// BootupDataplane cleans the NPM sets and policies in the dataplane and performs initialization.
// TODO rename this function to BootupDataplane
func (dp *DataPlane) BootupDataplane() error {
	// NOTE: used to create an all-namespaces set, but there's no need since it will be created by the control plane
	return dp.bootupDataPlane() //nolint:wrapcheck // unnecessary to wrap error
}

// RunPeriodicTasks runs periodic tasks. Should only be called once.
func (dp *DataPlane) RunPeriodicTasks() {
	go func() {
		ticker := time.NewTicker(time.Minute * time.Duration(reconcileTimeInMinutes))
		defer ticker.Stop()

		for {
			select {
			case <-dp.stopChannel:
				return
			case <-ticker.C:
				// send the heartbeat log in another go routine in case it takes a while
				go metrics.SendHeartbeatLog()

				// locks ipset manager
				dp.ipsetMgr.Reconcile()

				// in Windows, does nothing
				// in Linux, locks policy manager but can be interrupted
				dp.policyMgr.Reconcile()
			}
		}
	}()
}

func (dp *DataPlane) GetIPSet(setName string) *ipsets.IPSet {
	return dp.ipsetMgr.GetIPSet(setName)
}

// CreateIPSets takes in a set object and updates local cache with this set
func (dp *DataPlane) CreateIPSets(setMetadata []*ipsets.IPSetMetadata) {
	dp.ipsetMgr.CreateIPSets(setMetadata)
}

// DeleteSet checks for members and references of the given "set" type ipset
// if not used then will delete it from cache
func (dp *DataPlane) DeleteIPSet(setMetadata *ipsets.IPSetMetadata, forceDelete util.DeleteOption) {
	dp.ipsetMgr.DeleteIPSet(setMetadata.GetPrefixName(), forceDelete)
}

// AddToSets takes in a list of IPSet names along with IP member
// and then updates it local cache
func (dp *DataPlane) AddToSets(setNames []*ipsets.IPSetMetadata, podMetadata *PodMetadata) error {
	err := dp.ipsetMgr.AddToSets(setNames, podMetadata.PodIP, podMetadata.PodKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while adding to set: %w", err)
	}
	if dp.shouldUpdatePod() {
		klog.Infof("[DataPlane] Updating Sets to Add for pod key %s", podMetadata.PodKey)
		if _, ok := dp.updatePodCache[podMetadata.PodKey]; !ok {
			klog.Infof("[DataPlane] {AddToSet} pod key %s not found creating a new obj", podMetadata.PodKey)
			dp.updatePodCache[podMetadata.PodKey] = newUpdateNPMPod(podMetadata)
		}

		dp.updatePodCache[podMetadata.PodKey].updateIPSetsToAdd(setNames)
	}

	return nil
}

// RemoveFromSets takes in list of setnames from which a given IP member should be
// removed and will update the local cache
func (dp *DataPlane) RemoveFromSets(setNames []*ipsets.IPSetMetadata, podMetadata *PodMetadata) error {
	err := dp.ipsetMgr.RemoveFromSets(setNames, podMetadata.PodIP, podMetadata.PodKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while removing from set: %w", err)
	}

	if dp.shouldUpdatePod() {
		klog.Infof("[DataPlane] Updating Sets to Remove for pod key %s", podMetadata.PodKey)
		if _, ok := dp.updatePodCache[podMetadata.PodKey]; !ok {
			klog.Infof("[DataPlane] {RemoveFromSet} pod key %s not found creating a new obj", podMetadata.PodKey)
			dp.updatePodCache[podMetadata.PodKey] = newUpdateNPMPod(podMetadata)
		}

		dp.updatePodCache[podMetadata.PodKey].updateIPSetsToRemove(setNames)
	}

	return nil
}

// AddToLists takes a list name and list of sets which are to be added as members
// to given list
func (dp *DataPlane) AddToLists(listName, setNames []*ipsets.IPSetMetadata) error {
	err := dp.ipsetMgr.AddToLists(listName, setNames)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while adding to list: %w", err)
	}
	return nil
}

// RemoveFromList takes a list name and list of sets which are to be removed as members
// to given list
func (dp *DataPlane) RemoveFromList(listName *ipsets.IPSetMetadata, setNames []*ipsets.IPSetMetadata) error {
	err := dp.ipsetMgr.RemoveFromList(listName, setNames)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while removing from list: %w", err)
	}
	return nil
}

// ApplyDataPlane all the IPSet operations just update cache and update a dirty ipset structure,
// they do not change apply changes into dataplane. This function needs to be called at the
// end of IPSet operations of a given controller event, it will check for the dirty ipset list
// and accordingly makes changes in dataplane. This function helps emulate a single call to
// dataplane instead of multiple ipset operations calls ipset operations calls to dataplane
func (dp *DataPlane) ApplyDataPlane() error {
	err := dp.ipsetMgr.ApplyIPSets()
	if err != nil {
		return fmt.Errorf("[DataPlane] error while applying IPSets: %w", err)
	}

	if dp.shouldUpdatePod() {
		for podKey, pod := range dp.updatePodCache {
			err := dp.updatePod(pod)
			if err != nil {
				metrics.SendErrorLogAndMetric(util.DaemonDataplaneID, "error: failed to update pods: %s", err.Error())
				return fmt.Errorf("[DataPlane] error while updating pod: %w", err)
			}
			delete(dp.updatePodCache, podKey)
		}
	}
	return nil
}

// AddPolicy takes in a translated NPMNetworkPolicy object and applies on dataplane
func (dp *DataPlane) AddPolicy(policy *policies.NPMNetworkPolicy) error {
	klog.Infof("[DataPlane] Add Policy called for %s", policy.PolicyKey)
	// Create and add references for Selector IPSets first
	err := dp.createIPSetsAndReferences(policy.PodSelectorIPSets, policy.PolicyKey, ipsets.SelectorType)
	if err != nil {
		klog.Infof("[DataPlane] error while adding Selector IPSet references: %s", err.Error())
		return fmt.Errorf("[DataPlane] error while adding Selector IPSet references: %w", err)
	}

	// Create and add references for Rule IPSets
	err = dp.createIPSetsAndReferences(policy.RuleIPSets, policy.PolicyKey, ipsets.NetPolType)
	if err != nil {
		klog.Infof("[DataPlane] error while adding Rule IPSet references: %s", err.Error())
		return fmt.Errorf("[DataPlane] error while adding Rule IPSet references: %w", err)
	}

	err = dp.ApplyDataPlane()
	if err != nil {
		return fmt.Errorf("[DataPlane] error while applying dataplane: %w", err)
	}
	// TODO calculate endpoints to apply policy on
	endpointList, err := dp.getEndpointsToApplyPolicy(policy)
	if err != nil {
		return err
	}

	err = dp.policyMgr.AddPolicy(policy, endpointList)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while adding policy: %w", err)
	}
	return nil
}

// RemovePolicy takes in network policyKey (namespace/name of network policy) and removes it from dataplane and cache
func (dp *DataPlane) RemovePolicy(policyKey string) error {
	klog.Infof("[DataPlane] Remove Policy called for %s", policyKey)
	// because policy Manager will remove from policy from cache
	// keep a local copy to remove references for ipsets
	policy, ok := dp.policyMgr.GetPolicy(policyKey)
	if !ok {
		klog.Infof("[DataPlane] Policy %s is not found. Might been deleted already", policyKey)
		return nil
	}
	// Use the endpoint list saved in cache for this network policy to remove
	err := dp.policyMgr.RemovePolicy(policy.PolicyKey, nil)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while removing policy: %w", err)
	}
	// Remove references for Rule IPSets first
	err = dp.deleteIPSetsAndReferences(policy.RuleIPSets, policy.PolicyKey, ipsets.NetPolType)
	if err != nil {
		return err
	}

	// Remove references for Selector IPSets
	err = dp.deleteIPSetsAndReferences(policy.PodSelectorIPSets, policy.PolicyKey, ipsets.SelectorType)
	if err != nil {
		return err
	}

	err = dp.ApplyDataPlane()
	if err != nil {
		return fmt.Errorf("[DataPlane] error while applying dataplane: %w", err)
	}

	return nil
}

// UpdatePolicy takes in updated policy object, calculates the delta and applies changes
// onto dataplane accordingly
func (dp *DataPlane) UpdatePolicy(policy *policies.NPMNetworkPolicy) error {
	klog.Infof("[DataPlane] Update Policy called for %s", policy.PolicyKey)
	ok := dp.policyMgr.PolicyExists(policy.PolicyKey)
	if !ok {
		klog.Infof("[DataPlane] Policy %s is not found.", policy.PolicyKey)
		return dp.AddPolicy(policy)
	}

	// TODO it would be ideal to calculate a diff of policies
	// and remove/apply only the delta of IPSets and policies

	// Taking the easy route here, delete existing policy
	err := dp.RemovePolicy(policy.PolicyKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while updating policy: %w", err)
	}
	// and add the new updated policy
	err = dp.AddPolicy(policy)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while updating policy: %w", err)
	}
	return nil
}

func (dp *DataPlane) GetAllIPSets() []string {
	return dp.ipsetMgr.GetAllIPSets()
}

func (dp *DataPlane) GetAllPolicies() []string {
	return dp.policyMgr.GetAllPolicies()
}

func (dp *DataPlane) createIPSetsAndReferences(sets []*ipsets.TranslatedIPSet, netpolName string, referenceType ipsets.ReferenceType) error {
	// Create IPSets first along with reference updates
	npmErrorString := npmerrors.AddSelectorReference
	if referenceType == ipsets.NetPolType {
		npmErrorString = npmerrors.AddNetPolReference
	}
	for _, set := range sets {
		dp.ipsetMgr.CreateIPSets([]*ipsets.IPSetMetadata{set.Metadata})
		err := dp.ipsetMgr.AddReference(set.Metadata, netpolName, referenceType)
		if err != nil {
			return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to add reference with err: %s", err.Error()))
		}
	}

	// TODO is there a possibility for a list set of selector referencing rule ipset?
	// if so this below addition would throw an error because rule ipsets are not created
	// Check if any list sets are provided with members to add
	for _, set := range sets {
		// Check if any CIDR block IPSets needs to be applied
		setType := set.Metadata.Type
		if setType == ipsets.CIDRBlocks {
			// ipblock can have either cidr (CIDR in IPBlock) or "cidr + " " (space) + nomatch" (Except in IPBlock)
			// (TODO) need to revise it for windows
			for _, ipblock := range set.Members {
				err := ipsets.ValidateIPBlock(ipblock)
				if err != nil {
					return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to parseCIDR in addIPSetReferences with err: %s", err.Error()))
				}
				err = dp.ipsetMgr.AddToSets([]*ipsets.IPSetMetadata{set.Metadata}, ipblock, "")
				if err != nil {
					return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to AddToSet in addIPSetReferences with err: %s", err.Error()))
				}
			}
		} else if setType == ipsets.NestedLabelOfPod && len(set.Members) > 0 {
			// Check if any 2nd level IPSets are generated by Controller with members
			// Apply members to the list set
			err := dp.ipsetMgr.AddToLists([]*ipsets.IPSetMetadata{set.Metadata}, ipsets.GetMembersOfTranslatedSets(set.Members))
			if err != nil {
				return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to AddToList in addIPSetReferences with err: %s", err.Error()))
			}
		}
	}

	return nil
}

func (dp *DataPlane) deleteIPSetsAndReferences(sets []*ipsets.TranslatedIPSet, netpolName string, referenceType ipsets.ReferenceType) error {
	npmErrorString := npmerrors.DeleteSelectorReference
	if referenceType == ipsets.NetPolType {
		npmErrorString = npmerrors.DeleteNetPolReference
	}
	for _, set := range sets {
		// TODO ignore set does not exist error
		err := dp.ipsetMgr.DeleteReference(set.Metadata.GetPrefixName(), netpolName, referenceType)
		if err != nil {
			return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to deleteIPSetReferences with err: %s", err.Error()))
		}
	}

	// Check if any list sets are provided with members to add
	// TODO for nested IPsets check if we are safe to remove members
	// if k1:v0:v1 is created by two network policies
	// and both have same members
	// then we should not delete k1:v0:v1 members ( special case for nested ipsets )
	for _, set := range sets {
		// Check if any CIDR block IPSets needs to be applied
		setType := set.Metadata.Type
		if setType == ipsets.CIDRBlocks {
			// ipblock can have either cidr (CIDR in IPBlock) or "cidr + " " (space) + nomatch" (Except in IPBlock)
			// (TODO) need to revise it for windows
			for _, ipblock := range set.Members {
				err := ipsets.ValidateIPBlock(ipblock)
				if err != nil {
					return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to parseCIDR in deleteIPSetReferences with err: %s", err.Error()))
				}
				err = dp.ipsetMgr.RemoveFromSets([]*ipsets.IPSetMetadata{set.Metadata}, ipblock, "")
				if err != nil {
					return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to RemoveFromSet in deleteIPSetReferences with err: %s", err.Error()))
				}
			}
		} else if set.Metadata.GetSetKind() == ipsets.ListSet && len(set.Members) > 0 {
			// Delete if any 2nd level IPSets are generated by Controller with members
			err := dp.ipsetMgr.RemoveFromList(set.Metadata, ipsets.GetMembersOfTranslatedSets(set.Members))
			if err != nil {
				return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to RemoveFromList in deleteIPSetReferences with err: %s", err.Error()))
			}
		}

		// Try to delete these IPSets
		dp.ipsetMgr.DeleteIPSet(set.Metadata.GetPrefixName(), false)
	}
	return nil
}
