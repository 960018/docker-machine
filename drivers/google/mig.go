package google

// Managed Instance Group provisioning path.
//
// When the driver is configured with --google-instance-group-manager, the
// usual Instances.Insert / Instances.Delete calls are routed through the
// MIG's CreateInstances / DeleteInstances APIs. The MIG's instance template
// owns most of what would otherwise come from the driver flags (machine
// type, boot disk, network, image, labels, scheduling). Per-instance bits
// docker-machine still needs to drive — the SSH key, the firewall tag, the
// instance name — are applied after creation via the same path the direct
// flow uses.
//
// This is the gateway to Flex MIG: the template declares a precedence list
// of machine families, and GCP picks the first available one on each
// create. We don't implement the retry loop ourselves; docker-machine asks
// the MIG for one instance and gets one back (or an error), with the
// machine-family fallback hidden inside that one call.
//
// Scope of the prototype:
//   - Zonal MIG only (--google-instance-group-manager). Regional MIGs need
//     the RegionInstanceGroupManagers service + zone discovery from the
//     operation response; layered later if the prototype proves out.
//   - We trust the operator to leave any MIG-side autoscaler disabled.
//     CreateInstances / DeleteInstances implicitly bump and decrement
//     targetSize; a co-resident autoscaler would fight docker-machine.
//   - We log warnings (rather than rejecting) when flags overridden by the
//     instance template are set, so the prototype is drop-in for existing
//     configs.

import (
	"fmt"

	"github.com/docker/machine/libmachine/log"
	raw "google.golang.org/api/compute/v1"
)

// createInstanceViaMIG asks the configured MIG to provision an instance with
// the docker-machine-assigned name, waits for the operation to settle, then
// hands off to the existing post-create configuration (firewall tag, SSH key
// upload).
func (c *ComputeUtil) createInstanceViaMIG(d *Driver) error {
	log.Infof("Creating instance via Managed Instance Group %q", c.instanceGroupManager)
	c.warnAboutIgnoredFlagsInMIGMode(d)

	req := &raw.InstanceGroupManagersCreateInstancesRequest{
		Instances: []*raw.PerInstanceConfig{
			{Name: c.instanceName},
		},
	}

	op, err := c.service.InstanceGroupManagers.
		CreateInstances(c.project, c.zone, c.instanceGroupManager, req).
		Do()
	if err != nil {
		return fmt.Errorf("MIG %q rejected create for %q: %w", c.instanceGroupManager, c.instanceName, err)
	}

	log.Infof("Waiting for MIG create operation %s", op.Name)
	if err := c.waitForRegionalOp(op.Name); err != nil {
		return fmt.Errorf("MIG %q create for %q did not complete: %w", c.instanceGroupManager, c.instanceName, err)
	}

	// At this point the MIG has scheduled instance creation; the VM exists.
	// Fall through to the same post-create steps the direct path runs.
	instance, err := c.instance()
	if err != nil {
		return fmt.Errorf("looking up just-created MIG instance %q: %w", c.instanceName, err)
	}

	if err := c.addFirewallTag(instance); err != nil {
		return fmt.Errorf("adding firewall tag to MIG instance %q: %w", c.instanceName, err)
	}

	return c.uploadSSHKey(instance, d.GetSSHKeyPath())
}

// deleteInstanceViaMIG removes the instance through the MIG so targetSize
// and per-instance configs are kept consistent. Boot disks are owned by the
// MIG's template, so the caller's subsequent deleteDisk() lookup will find
// nothing and become a no-op.
func (c *ComputeUtil) deleteInstanceViaMIG() error {
	log.Infof("Deleting instance via Managed Instance Group %q", c.instanceGroupManager)

	instanceURL := fmt.Sprintf("zones/%s/instances/%s", c.zone, c.instanceName)
	req := &raw.InstanceGroupManagersDeleteInstancesRequest{
		Instances:                      []string{instanceURL},
		SkipInstancesOnValidationError: true, // tolerate races with manual cleanup
	}

	op, err := c.service.InstanceGroupManagers.
		DeleteInstances(c.project, c.zone, c.instanceGroupManager, req).
		Do()
	if err != nil {
		return fmt.Errorf("MIG %q rejected delete for %q: %w", c.instanceGroupManager, c.instanceName, err)
	}

	log.Infof("Waiting for MIG delete operation %s", op.Name)
	return c.waitForRegionalOp(op.Name)
}

// warnAboutIgnoredFlagsInMIGMode walks the driver flags that the instance
// template overrides and logs a single warning per non-default value. The
// goal is to make migration friction visible — operators copying a working
// non-MIG config into MIG mode will see exactly which flags are now no-ops.
//
// We warn rather than reject so the prototype is drop-in for existing chef
// roles; tightening to a hard error is a follow-up once the migration story
// is clearer.
func (c *ComputeUtil) warnAboutIgnoredFlagsInMIGMode(d *Driver) {
	ignored := map[string]bool{
		"google-machine-type":    d.MachineType != "" && d.MachineType != defaultMachineType,
		"google-machine-image":   d.MachineImage != "" && d.MachineImage != defaultImageName,
		"google-disk-size":       d.DiskSize != defaultDiskSize,
		"google-disk-type":       d.DiskType != "" && d.DiskType != defaultDiskType,
		"google-network":         d.Network != "" && d.Network != defaultNetwork,
		"google-subnetwork":      d.Subnetwork != "",
		"google-min-cpu-platform": d.MinCPUPlatform != "",
		"google-accelerator":     d.Accelerator != "",
		"google-preemptible":     d.Preemptible,
		"google-service-account": d.ServiceAccount != "" && d.ServiceAccount != defaultServiceAccount,
	}
	for flag, set := range ignored {
		if set {
			log.Warnf("Flag --%s is ignored in MIG mode; the MIG %q's instance template owns this setting", flag, c.instanceGroupManager)
		}
	}
}
