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
//   - Zonal MIG (--google-instance-group-manager) and regional MIG
//     (--google-region-instance-group-manager + --google-region) are
//     supported. Regional MIGs require an extra step after create: the
//     zone the MIG placed the instance in is unknown until we list managed
//     instances and parse it out of the instance URL. We then mutate
//     c.zone (and the underlying Driver.Zone) so subsequent per-instance
//     operations (SetTags, SetMetadata, eventually Delete after a Driver
//     reload from disk) target the right zone.
//   - We trust the operator to leave any MIG-side autoscaler disabled.
//     CreateInstances / DeleteInstances implicitly bump and decrement
//     targetSize; a co-resident autoscaler would fight docker-machine.
//   - We log warnings (rather than rejecting) when flags overridden by the
//     instance template are set, so the prototype is drop-in for existing
//     configs.

import (
	"fmt"
	"strings"

	"github.com/docker/machine/libmachine/log"
	raw "google.golang.org/api/compute/v1"
)

// createInstanceViaMIG asks the configured MIG to provision an instance with
// the docker-machine-assigned name, waits for the operation to settle, then
// hands off to the existing post-create configuration (firewall tag, SSH key
// upload). Dispatches between the zonal and regional MIG flavours.
func (c *ComputeUtil) createInstanceViaMIG(d *Driver) error {
	c.warnAboutIgnoredFlagsInMIGMode(d)

	if c.usesRegionalMIG() {
		return c.createInstanceViaRegionalMIG(d)
	}
	return c.createInstanceViaZonalMIG(d)
}

func (c *ComputeUtil) createInstanceViaZonalMIG(d *Driver) error {
	log.Infof("Creating instance via zonal Managed Instance Group %q", c.instanceGroupManager)

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

	return c.finishPostCreate(d)
}

func (c *ComputeUtil) createInstanceViaRegionalMIG(d *Driver) error {
	log.Infof("Creating instance via regional Managed Instance Group %q in %q", c.regionInstanceGroupManager, c.region())

	req := &raw.RegionInstanceGroupManagersCreateInstancesRequest{
		Instances: []*raw.PerInstanceConfig{
			{Name: c.instanceName},
		},
	}

	op, err := c.service.RegionInstanceGroupManagers.
		CreateInstances(c.project, c.region(), c.regionInstanceGroupManager, req).
		Do()
	if err != nil {
		return fmt.Errorf("regional MIG %q rejected create for %q: %w", c.regionInstanceGroupManager, c.instanceName, err)
	}

	log.Infof("Waiting for regional MIG create operation %s", op.Name)
	if err := c.waitForRegionOp(op.Name); err != nil {
		return fmt.Errorf("regional MIG %q create for %q did not complete: %w", c.regionInstanceGroupManager, c.instanceName, err)
	}

	// Discover which zone the regional MIG placed the instance in. The
	// CreateInstances Operation returned above is region-scoped and its
	// TargetLink points at the MIG, not the instance, so there's no
	// placement info in the response. ListManagedInstances is the
	// after-the-fact lookup; an OperationGroupId-filtered RegionOperations
	// list would also work and is more targeted, but at MIG sizes we
	// expect the cost is a wash.
	//
	// c.zone is set so the rest of *this* call (finishPostCreate →
	// instance() / addFirewallTag / uploadSSHKey) targets the right zone.
	// d.Zone is set so libmachine's Filestore.Save persists it across the
	// process boundary — every docker-machine subcommand is a fresh
	// process that constructs ComputeUtil from the on-disk Driver state,
	// so subsequent invocations (rm, ip, status, …) read the discovered
	// zone back from disk and don't need to re-resolve.
	zone, err := c.resolveInstanceZoneFromRegionalMIG()
	if err != nil {
		return fmt.Errorf("discovering zone for MIG-placed instance %q: %w", c.instanceName, err)
	}
	c.zone = zone
	d.Zone = zone

	return c.finishPostCreate(d)
}

// finishPostCreate runs the create-time work that's identical between the
// direct path and any MIG flavour: look the instance up, add the firewall
// tag, push the SSH public key.
func (c *ComputeUtil) finishPostCreate(d *Driver) error {
	instance, err := c.instance()
	if err != nil {
		return fmt.Errorf("looking up just-created MIG instance %q: %w", c.instanceName, err)
	}

	if err := c.addFirewallTag(instance); err != nil {
		return fmt.Errorf("adding firewall tag to MIG instance %q: %w", c.instanceName, err)
	}

	return c.uploadSSHKey(instance, d.GetSSHKeyPath())
}

// resolveInstanceZoneFromRegionalMIG looks up the zone the MIG placed our
// instance in. The CreateInstances response doesn't carry that — the
// returned Operation is region-scoped and its TargetLink points at the
// MIG, not the spawned instance.
//
// Uses ListManagedInstances with a server-side filter (`name eq "..."`).
// Two things to know about this endpoint:
//
//   - The filter runs server-side, so the response is bounded to the
//     matches regardless of MIG size. Critical at our scale: without it
//     the endpoint returns the MIG's full instance set in a single
//     response (the default listManagedInstancesResults setting is
//     PAGELESS, which disables maxResults/pageToken pagination — so on
//     a default MIG you'd be pulling hundreds of entries per create
//     just to find one zone string).
//   - It's a POST despite being a "list". Easy footgun for hand-rolled
//     HTTP; the Go SDK handles the method correctly so nothing to do
//     here beyond know it.
//
// Scope-wise this is naturally tighter than aggregatedList: a regional
// MIG belongs to exactly one region, so there's no cross-region name-
// collision concern to defend against client-side.
func (c *ComputeUtil) resolveInstanceZoneFromRegionalMIG() (string, error) {
	resp, err := c.service.RegionInstanceGroupManagers.
		ListManagedInstances(c.project, c.region(), c.regionInstanceGroupManager).
		Filter(fmt.Sprintf("name eq %q", c.instanceName)).
		Do()
	if err != nil {
		return "", fmt.Errorf("listManagedInstances lookup for %q in MIG %q: %w", c.instanceName, c.regionInstanceGroupManager, err)
	}
	if len(resp.ManagedInstances) == 0 {
		return "", fmt.Errorf("instance %q not found in regional MIG %q after create (the operation completed but ListManagedInstances did not return it)", c.instanceName, c.regionInstanceGroupManager)
	}
	// Server-side filter narrows to instances whose name equals ours; the
	// Instance field is the full self-link
	// (https://.../zones/<zone>/instances/<name>) and zoneFromInstanceURL
	// pulls out the zone segment.
	return zoneFromInstanceURL(resp.ManagedInstances[0].Instance)
}

// zoneFromInstanceURL parses the zone segment from a compute self-link of
// the form ".../zones/<zone>/instances/<name>".
func zoneFromInstanceURL(url string) (string, error) {
	const marker = "/zones/"
	idx := strings.Index(url, marker)
	if idx == -1 {
		return "", fmt.Errorf("no %q segment in instance URL %q", marker, url)
	}
	rest := url[idx+len(marker):]
	end := strings.Index(rest, "/")
	if end == -1 {
		return "", fmt.Errorf("malformed instance URL %q (no path component after zone)", url)
	}
	return rest[:end], nil
}

// deleteInstanceViaMIG removes the instance through the MIG so targetSize
// and per-instance configs are kept consistent. Boot disks are owned by the
// MIG's template, so the caller's subsequent deleteDisk() lookup will find
// nothing and become a no-op. Dispatches between zonal and regional.
func (c *ComputeUtil) deleteInstanceViaMIG() error {
	if c.usesRegionalMIG() {
		return c.deleteInstanceViaRegionalMIG()
	}
	return c.deleteInstanceViaZonalMIG()
}

func (c *ComputeUtil) deleteInstanceViaZonalMIG() error {
	log.Infof("Deleting instance via zonal Managed Instance Group %q", c.instanceGroupManager)

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

func (c *ComputeUtil) deleteInstanceViaRegionalMIG() error {
	log.Infof("Deleting instance via regional Managed Instance Group %q in %q", c.regionInstanceGroupManager, c.region())

	// We need the zone to build the instance self-link in the delete
	// request. On the Create path we wrote the discovered zone to
	// Driver.Zone; libmachine persisted that to disk, and this fresh
	// invocation read it back into c.zone via newComputeUtil. If for some
	// reason the persisted state was lost (interrupted create, manual
	// cleanup of config.json, …), fall back to a fresh
	// ListManagedInstances lookup.
	zone := c.zone
	if zone == "" {
		discovered, err := c.resolveInstanceZoneFromRegionalMIG()
		if err != nil {
			return fmt.Errorf("discovering zone before regional MIG delete: %w", err)
		}
		zone = discovered
	}

	instanceURL := fmt.Sprintf("zones/%s/instances/%s", zone, c.instanceName)
	req := &raw.RegionInstanceGroupManagersDeleteInstancesRequest{
		Instances:                      []string{instanceURL},
		SkipInstancesOnValidationError: true,
	}

	op, err := c.service.RegionInstanceGroupManagers.
		DeleteInstances(c.project, c.region(), c.regionInstanceGroupManager, req).
		Do()
	if err != nil {
		return fmt.Errorf("regional MIG %q rejected delete for %q: %w", c.regionInstanceGroupManager, c.instanceName, err)
	}

	log.Infof("Waiting for regional MIG delete operation %s", op.Name)
	return c.waitForRegionOp(op.Name)
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
		"google-machine-type":     d.MachineType != "" && d.MachineType != defaultMachineType,
		"google-machine-image":    d.MachineImage != "" && d.MachineImage != defaultImageName,
		"google-disk-size":        d.DiskSize != defaultDiskSize,
		"google-disk-type":        d.DiskType != "" && d.DiskType != defaultDiskType,
		"google-network":          d.Network != "" && d.Network != defaultNetwork,
		"google-subnetwork":       d.Subnetwork != "",
		"google-min-cpu-platform": d.MinCPUPlatform != "",
		"google-accelerator":      d.Accelerator != "",
		"google-preemptible":      d.Preemptible,
		"google-service-account":  d.ServiceAccount != "" && d.ServiceAccount != defaultServiceAccount,
	}
	// Regional MIG places its own zone; the operator-supplied --google-zone
	// would conflict. Warn so the configuration error surfaces.
	if d.usesRegionalMIG() && d.Zone != "" && d.Zone != defaultZone {
		ignored["google-zone"] = true
	}
	migName := c.instanceGroupManager
	if c.usesRegionalMIG() {
		migName = c.regionInstanceGroupManager
	}
	for flag, set := range ignored {
		if set {
			log.Warnf("Flag --%s is ignored in MIG mode; the MIG %q's instance template owns this setting", flag, migName)
		}
	}
}
