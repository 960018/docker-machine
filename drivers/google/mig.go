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
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/docker/machine/libmachine/log"
	raw "google.golang.org/api/compute/v1"
)

// zonePathSegment matches the "/zones/<zone>" segment of a GCE
// self-link path. The trailing slash is optional so the same regex
// matches both:
//   - Instance self-links: ".../zones/<zone>/instances/<name>"
//   - Zone self-links:     ".../zones/<zone>"
//
// Precompiled at package init so zoneFromInstanceURL doesn't pay the
// compile cost per call.
var zonePathSegment = regexp.MustCompile(`/zones/([^/]+)`)

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

	// The Operation completing only signals that the MIG accepted the
	// request and started provisioning; the VM is not yet visible to
	// Instances.Get. Wait for the MIG to mark the instance stable
	// (CurrentAction == "NONE") before finishPostCreate's lookups race.
	if _, err := c.waitForZonalMIGInstanceReady(); err != nil {
		return fmt.Errorf("waiting for MIG-placed instance %q to be ready: %w", c.instanceName, err)
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

	// Two things to do after the region Operation completes:
	//
	//   1. Wait for the MIG to mark the instance stable. The Operation
	//      completing only signals that the MIG accepted the request and
	//      started provisioning; the VM is not yet visible to Instances.Get,
	//      so finishPostCreate's lookup races without this wait.
	//
	//   2. Discover which zone the regional MIG placed the instance in.
	//      The Operation's TargetLink points at the MIG, not the instance,
	//      so there's no placement info in the response. The same
	//      ListManagedInstances poll that gives us stability gives us the
	//      instance self-link, from which we pull the zone segment.
	//
	// c.zone is set so the rest of *this* call (finishPostCreate →
	// instance() / addFirewallTag / uploadSSHKey) targets the right zone.
	// d.ResolvedZone is set so libmachine's Filestore.Save persists it
	// across the process boundary — every docker-machine subcommand is a
	// fresh process that constructs ComputeUtil from the on-disk Driver
	// state, so subsequent invocations (rm, ip, status, …) read the
	// discovered zone back from disk via the ResolvedZone-aware fallback
	// in newComputeUtil.
	mi, err := c.waitForRegionalMIGInstanceReady()
	if err != nil {
		return fmt.Errorf("waiting for MIG-placed instance %q to be ready: %w", c.instanceName, err)
	}
	zone, err := zoneFromInstanceURL(mi.Instance)
	if err != nil {
		return fmt.Errorf("discovering zone for MIG-placed instance %q: %w", c.instanceName, err)
	}
	c.zone = zone
	d.ResolvedZone = zone

	return c.finishPostCreate(d)
}

// finishPostCreate runs the create-time work that's identical between the
// direct path and any MIG flavour: look the instance up, sync the
// discovered placement back into Driver state so libmachine persists it,
// add the firewall tag, push the SSH public key.
func (c *ComputeUtil) finishPostCreate(d *Driver) error {
	instance, err := c.instance()
	if err != nil {
		return fmt.Errorf("looking up just-created MIG instance %q: %w", c.instanceName, err)
	}

	// The MIG (and Flex MIG in particular) owns the machine-type and
	// zone placement; the operator's --google-* flags are ignored. Write
	// the actual values back to Driver so they get persisted to
	// config.json and metric labels downstream (gitlab-runner!6740's
	// target_machine_type / target_zone) reflect reality rather than
	// the operator's input.
	c.syncDriverStateFromInstance(d, instance)

	if err := c.addFirewallTag(instance); err != nil {
		return fmt.Errorf("adding firewall tag to MIG instance %q: %w", c.instanceName, err)
	}

	return c.uploadSSHKey(instance, d.GetSSHKeyPath())
}

// syncDriverStateFromInstance populates Driver.ResolvedZone and
// Driver.ResolvedMachineType from a just-created instance. Called after
// a successful create from a MIG path so the resolved values get
// persisted by libmachine alongside the operator-intent Zone /
// MachineType fields.
//
// Best-effort: parsing failures log and leave the field empty rather
// than blocking post-create. The empty case (no resolved value) is
// handled by ComputeUtil's zone fallback (ResolvedZone || Zone) and
// by gitlab-runner!6740 (ResolvedMachineType || MachineType).
func (c *ComputeUtil) syncDriverStateFromInstance(d *Driver, instance *raw.Instance) {
	if instance == nil {
		return
	}
	if mt, err := machineTypeFromInstanceURL(instance.MachineType); err == nil {
		d.ResolvedMachineType = mt
	} else {
		log.Warnf("could not parse machine type from instance URL %q: %s", instance.MachineType, err)
	}
	if zone, err := zoneFromInstanceURL(instance.Zone); err == nil {
		d.ResolvedZone = zone
		c.zone = zone
	} else {
		log.Warnf("could not parse zone from instance URL %q: %s", instance.Zone, err)
	}
}

// machineTypeFromInstanceURL parses the machine-type segment from a
// compute self-link of the form ".../machineTypes/<type>". Same shape as
// zoneFromInstanceURL: net/url for fail-fast validation, regex for the
// segment extraction.
var machineTypePathSegment = regexp.MustCompile(`/machineTypes/([^/]+)$`)

func machineTypeFromInstanceURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("machine-type URL %q: %w", rawURL, err)
	}
	m := machineTypePathSegment.FindStringSubmatch(u.Path)
	if m == nil {
		return "", fmt.Errorf("machine-type URL %q has no /machineTypes/<type> segment", rawURL)
	}
	return m[1], nil
}

// waitForZonalMIGInstanceReady polls ListManagedInstances on the zonal
// MIG until the entry for our instance reports CurrentAction == "NONE".
// See waitForMIGInstanceReady for why this exists.
func (c *ComputeUtil) waitForZonalMIGInstanceReady() (*raw.ManagedInstance, error) {
	return c.waitForMIGInstanceReady(c.instanceGroupManager, func() ([]*raw.ManagedInstance, error) {
		resp, err := c.service.InstanceGroupManagers.
			ListManagedInstances(c.project, c.zone, c.instanceGroupManager).
			Filter(fmt.Sprintf("name eq %q", c.instanceName)).
			Do()
		if err != nil {
			return nil, fmt.Errorf("listManagedInstances lookup for %q in zonal MIG %q: %w", c.instanceName, c.instanceGroupManager, err)
		}
		return resp.ManagedInstances, nil
	})
}

// waitForRegionalMIGInstanceReady polls ListManagedInstances on the
// regional MIG until the entry for our instance reports CurrentAction ==
// "NONE", and returns that entry. Callers use the entry's Instance URL
// to discover the zone the MIG placed the instance in.
//
// Two things to know about this listManagedInstances endpoint:
//
//   - The filter runs server-side, so the response is bounded to the
//     matches regardless of MIG size. Critical at scale: without it the
//     endpoint returns the MIG's full instance set in a single response
//     (the default listManagedInstancesResults setting is PAGELESS,
//     which disables maxResults/pageToken pagination — so on a default
//     MIG you'd be pulling hundreds of entries per poll iteration just
//     to find one entry).
//   - It's a POST despite being a "list". Easy footgun for hand-rolled
//     HTTP; the Go SDK handles the method correctly.
//
// Scope-wise this is naturally tighter than aggregatedList: a regional
// MIG belongs to exactly one region, so there's no cross-region name-
// collision concern to defend against client-side.
func (c *ComputeUtil) waitForRegionalMIGInstanceReady() (*raw.ManagedInstance, error) {
	return c.waitForMIGInstanceReady(c.regionInstanceGroupManager, func() ([]*raw.ManagedInstance, error) {
		resp, err := c.service.RegionInstanceGroupManagers.
			ListManagedInstances(c.project, c.region(), c.regionInstanceGroupManager).
			Filter(fmt.Sprintf("name eq %q", c.instanceName)).
			Do()
		if err != nil {
			return nil, fmt.Errorf("listManagedInstances lookup for %q in regional MIG %q: %w", c.instanceName, c.regionInstanceGroupManager, err)
		}
		return resp.ManagedInstances, nil
	})
}

// lookupInstanceZoneInRegionalMIG does a single ListManagedInstances call
// to find the zone our instance currently lives in. Used by the delete
// path as a fallback when the zone wasn't persisted to disk during create
// (interrupted create, manual state cleanup, etc.). Unlike the create
// path's waitForRegionalMIGInstanceReady, this does *not* wait for
// CurrentAction == "NONE": a mid-create or mid-delete instance still
// has a zone we can target for deletion.
func (c *ComputeUtil) lookupInstanceZoneInRegionalMIG() (string, error) {
	resp, err := c.service.RegionInstanceGroupManagers.
		ListManagedInstances(c.project, c.region(), c.regionInstanceGroupManager).
		Filter(fmt.Sprintf("name eq %q", c.instanceName)).
		Do()
	if err != nil {
		return "", fmt.Errorf("listManagedInstances lookup for %q in regional MIG %q: %w", c.instanceName, c.regionInstanceGroupManager, err)
	}
	if len(resp.ManagedInstances) == 0 {
		return "", fmt.Errorf("instance %q not found in regional MIG %q", c.instanceName, c.regionInstanceGroupManager)
	}
	return zoneFromInstanceURL(resp.ManagedInstances[0].Instance)
}

// waitForMIGInstanceReady polls a MIG's listManagedInstances endpoint
// until the entry for our instance has CurrentAction == "NONE", meaning
// the underlying VM is fully provisioned and reliably visible to
// Instances.Get.
//
// The Operation returned by CreateInstances completing only signals that
// the MIG has accepted the request and started provisioning; the VM in
// compute.zones.instances is not yet visible. Without this wait the
// synchronous post-create steps (Instances.Get, SetTags for firewall,
// SetMetadata for SSH key) race against an in-flight create and 404.
//
// fleeting-plugin-googlecloud avoids this race by design: its Increase
// just fires CreateInstances and returns, and instance state is
// discovered asynchronously in periodic Update() ticks. docker-machine's
// CLI shape forces a synchronous create+configure+return flow per
// invocation, so we poll here instead.
//
// Reuses operationBackoffFactory so the wait shape (1s → 30s, 5min cap
// by default, all tunable via the --google-operation-backoff-* flags)
// matches the existing wait-for-operation behaviour.
func (c *ComputeUtil) waitForMIGInstanceReady(mig string, list func() ([]*raw.ManagedInstance, error)) (*raw.ManagedInstance, error) {
	if c.operationBackoffFactory == nil {
		return nil, errors.New("operationBackoffFactory is not defined")
	}
	b := c.operationBackoffFactory.create()
	b.Reset()

	for {
		instances, err := list()
		if err != nil {
			return nil, err
		}
		if len(instances) == 0 {
			return nil, fmt.Errorf("instance %q not found in MIG %q after create (the operation completed but listManagedInstances did not return it)", c.instanceName, mig)
		}
		mi := instances[0]
		if mi.CurrentAction == "NONE" {
			return mi, nil
		}
		log.Debugf("waiting for MIG instance %q (currentAction=%s, instanceStatus=%s)", c.instanceName, mi.CurrentAction, mi.InstanceStatus)

		next := b.NextBackOff()
		if next == backoff.Stop {
			return nil, fmt.Errorf("MIG instance %q in %q did not reach a stable state (last currentAction=%q) within backoff", c.instanceName, mig, mi.CurrentAction)
		}
		time.Sleep(next)
	}
}

// zoneFromInstanceURL parses the zone segment from a compute self-link of
// the form ".../zones/<zone>/instances/<name>". Parses with net/url first
// so genuinely-malformed input fails fast rather than slipping past
// substring matching, then pulls the zone via a tight regex on the path.
func zoneFromInstanceURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("instance URL %q: %w", rawURL, err)
	}
	m := zonePathSegment.FindStringSubmatch(u.Path)
	if m == nil {
		return "", fmt.Errorf("instance URL %q has no /zones/<zone>/ segment in its path", rawURL)
	}
	return m[1], nil
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
		// One-shot lookup; we don't want to block on CurrentAction == NONE
		// because the instance may already be mid-delete or in a failed
		// state, and we just need its zone to build the delete URL.
		discovered, err := c.lookupInstanceZoneInRegionalMIG()
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
