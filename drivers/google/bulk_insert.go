package google

// bulkInsert provisioning path.
//
// When the driver is configured with --google-region, the usual zonal
// Instances.Insert call is replaced by a regional RegionInstances.BulkInsert.
// The single request can carry two independent fallback policies:
//
//   - LocationPolicy: zone preference list. Each --google-location-zone
//     contributes one entry; GCP picks among ALLOW / PREFERRED zones (and
//     skips DENY zones) based on current capacity. Empty list = any zone
//     in the region.
//   - InstanceFlexibilityPolicy: ranked machine-type alternatives. Each
//     --google-flex-machine-type contributes one selection, with rank
//     assigned by occurrence order (first = rank 0 = most preferred). GCP
//     attempts the first-rank type and falls back to the next on capacity
//     errors. Empty list = use --google-machine-type as the single shape.
//
// Together these give per-create zone × machine-type spillover in one
// API call, without the operator having to provision a MIG. The
// docker-machine flow is unchanged otherwise: count=1, the instance is
// addressed by the caller-chosen name, and post-create configuration
// (firewall tag, SSH key) runs against the discovered zone.
//
// The operation returned by BulkInsert is region-scoped and completes
// once GCP has settled placement and started the create. As with the
// regional MIG path before this, we need a second step to discover the
// chosen zone before the synchronous post-create lookups race.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/docker/machine/libmachine/log"
	raw "google.golang.org/api/compute/v1"
)

// zonePathSegment matches the "/zones/<zone>" segment of a GCE
// self-link path. Used by zoneFromInstanceURL to extract placement
// from an instance's self-link returned by AggregatedList.
var zonePathSegment = regexp.MustCompile(`/zones/([^/]+)`)

// machineTypePathSegment matches the ".../machineTypes/<type>" tail of
// an instance's machineType self-link. Used by machineTypeFromInstanceURL
// to write the resolved machine type back to Driver state for metric
// labels downstream.
var machineTypePathSegment = regexp.MustCompile(`/machineTypes/([^/]+)$`)

// createInstanceViaBulkInsert provisions a single VM via
// RegionInstances.BulkInsert with the configured location and flex
// policies, waits for the operation to complete, discovers the chosen
// zone, and hands off to the standard post-create configuration.
func (c *ComputeUtil) createInstanceViaBulkInsert(d *Driver) error {
	c.warnAboutIgnoredFlagsInBulkInsertMode(d)

	log.Infof("Creating instance via bulkInsert in %q", c.region())

	props, err := c.buildBulkInsertInstanceProperties(d)
	if err != nil {
		return fmt.Errorf("building instance properties for bulkInsert: %w", err)
	}

	req := &raw.BulkInsertInstanceResource{
		Count:    1,
		MinCount: 1,
		PerInstanceProperties: map[string]raw.BulkInsertInstanceResourcePerInstanceProperties{
			c.instanceName: {},
		},
		InstanceProperties: props,
	}
	if policy := c.buildLocationPolicy(); policy != nil {
		req.LocationPolicy = policy
	}
	if flex := c.buildInstanceFlexibilityPolicy(); flex != nil {
		req.InstanceFlexibilityPolicy = flex
	}

	op, err := c.service.RegionInstances.BulkInsert(c.project, c.region(), req).Do()
	if err != nil {
		return fmt.Errorf("bulkInsert rejected create for %q in %q: %w", c.instanceName, c.region(), err)
	}

	log.Infof("Waiting for bulkInsert operation %s", op.Name)
	if err := c.waitForRegionOp(op.Name); err != nil {
		return fmt.Errorf("bulkInsert for %q did not complete: %w", c.instanceName, err)
	}

	// The region Operation completing signals GCP has settled placement
	// and started the create. To target subsequent calls (Instances.Get,
	// SetTags, SetMetadata) we need the zone the VM landed in. The
	// Operation's TargetLink isn't reliable for that, so locate the
	// instance by name via AggregatedList and parse the zone from its
	// self-link.
	zone, err := c.discoverInstanceZone()
	if err != nil {
		return fmt.Errorf("discovering zone for bulkInsert-placed instance %q: %w", c.instanceName, err)
	}
	c.zone = zone
	d.ResolvedZone = zone
	c.zoneURL = apiURL + c.project + "/zones/" + zone

	return c.finishPostCreate(d)
}

// buildBulkInsertInstanceProperties assembles the InstanceProperties
// template used by bulkInsert. Mirrors the body the direct path builds
// for Instances.Insert, but typed as InstanceProperties (which lacks
// the Name / Zone fields the direct path sets — those come from the
// bulkInsert request envelope / placement decision).
func (c *ComputeUtil) buildBulkInsertInstanceProperties(d *Driver) (*raw.InstanceProperties, error) {
	var net string
	if strings.Contains(d.Network, "/networks/") {
		net = d.Network
	} else {
		net = c.globalURL + "/networks/" + d.Network
	}

	metadata, err := prepareMetadata(d)
	if err != nil {
		return nil, err
	}

	// MachineType in the template is a bare type name (not a zonal URL):
	// bulkInsert resolves it against the actual placement zone. If a
	// flex policy is set the per-selection MachineTypes override this.
	props := &raw.InstanceProperties{
		Description:    "docker host vm",
		MachineType:    d.MachineType,
		MinCpuPlatform: c.minCPUPlatform,
		Disks: []*raw.AttachedDisk{
			{
				Boot:       true,
				AutoDelete: true,
				Type:       "PERSISTENT",
				Mode:       "READ_WRITE",
				InitializeParams: &raw.AttachedDiskInitializeParams{
					SourceImage: "https://www.googleapis.com/compute/v1/projects/" + d.MachineImage,
					DiskSizeGb:  int64(d.DiskSize),
					// Bare disk-type name. bulkInsert evaluates this
					// against the chosen zone post-placement; passing a
					// zonal URL here would either over-constrain
					// placement or be rejected as inconsistent.
					DiskType: c.diskTypeURL,
					Labels:   parseLabels(d),
				},
			},
		},
		NetworkInterfaces: []*raw.NetworkInterface{
			{Network: net},
		},
		Tags: &raw.Tags{
			Items: parseTags(d),
		},
		ServiceAccounts: []*raw.ServiceAccount{
			{
				Email:  d.ServiceAccount,
				Scopes: strings.Split(d.Scopes, ","),
			},
		},
		Scheduling: &raw.Scheduling{
			Preemptible: c.preemptible,
		},
		Labels:   parseLabels(d),
		Metadata: metadata,
	}

	if d.ProvisionedIops > 0 {
		props.Disks[0].InitializeParams.ProvisionedIops = int64(d.ProvisionedIops)
	}
	if d.ProvisionedThroughput > 0 {
		props.Disks[0].InitializeParams.ProvisionedThroughput = int64(d.ProvisionedThroughput)
	}

	if c.maintenancePolicy != "" {
		props.Scheduling.OnHostMaintenance = c.maintenancePolicy
	}

	if strings.Contains(c.subnetwork, "/subnetworks/") {
		props.NetworkInterfaces[0].Subnetwork = c.subnetwork
	} else if c.subnetwork != "" {
		props.NetworkInterfaces[0].Subnetwork = "projects/" + c.networkProject + "/regions/" + c.region() + "/subnetworks/" + c.subnetwork
	}

	if !c.useInternalIPOnly {
		props.NetworkInterfaces[0].AccessConfigs = append(props.NetworkInterfaces[0].AccessConfigs, &raw.AccessConfig{
			Type: "ONE_TO_ONE_NAT",
		})
	}

	// Note: --google-address (static IP), --google-accelerator. Both
	// imply a specific zone (acceleratorType is a zonal resource, static
	// addresses are regional but pin the NIC), which conflicts with
	// bulkInsert's zone-at-placement-time semantics. The flag validator
	// rejects them with --google-region; if we reach here they were
	// silently dropped from the request body to avoid an API error.

	return props, nil
}

// buildLocationPolicy returns a LocationPolicy assembled from the
// --google-location-zone flags, or nil when the operator left them
// empty (GCP then picks freely within the region).
func (c *ComputeUtil) buildLocationPolicy() *raw.LocationPolicy {
	if len(c.locationZones) == 0 && c.locationTargetShape == "" {
		return nil
	}

	policy := &raw.LocationPolicy{}
	if c.locationTargetShape != "" {
		policy.TargetShape = c.locationTargetShape
	}
	if len(c.locationZones) > 0 {
		policy.Locations = make(map[string]raw.LocationPolicyLocation, len(c.locationZones))
		for _, entry := range c.locationZones {
			zone, pref := parseLocationZoneEntry(entry)
			policy.Locations["zones/"+zone] = raw.LocationPolicyLocation{
				Preference: pref,
			}
		}
	}
	return policy
}

// parseLocationZoneEntry splits a "zone[:preference]" entry into its
// parts, defaulting preference to ALLOW when unset. Unknown preferences
// are passed through to GCP (which will reject the request) rather than
// silently coerced — surfacing the typo as an API error keeps the
// configuration contract honest.
func parseLocationZoneEntry(entry string) (zone, preference string) {
	if i := strings.IndexByte(entry, ':'); i >= 0 {
		return entry[:i], strings.ToUpper(entry[i+1:])
	}
	return entry, "ALLOW"
}

// buildInstanceFlexibilityPolicy assembles the flex policy from the
// --google-flex-machine-type flag, or returns nil when no flex
// selections were configured (the bulkInsert call then uses the single
// machine type from InstanceProperties.MachineType).
//
// Each occurrence of the flag becomes one InstanceSelection with rank
// equal to its 0-indexed position. The selection's key is an arbitrary
// RFC1035 string; we use "rank-N" so server-side error messages refer
// back to the operator's input order.
func (c *ComputeUtil) buildInstanceFlexibilityPolicy() *raw.InstanceFlexibilityPolicy {
	if len(c.flexMachineTypes) == 0 {
		return nil
	}

	selections := make(map[string]raw.InstanceFlexibilityPolicyInstanceSelection, len(c.flexMachineTypes))
	for i, mt := range c.flexMachineTypes {
		key := fmt.Sprintf("rank-%d", i)
		selections[key] = raw.InstanceFlexibilityPolicyInstanceSelection{
			MachineTypes: []string{mt},
			Rank:         int64(i),
		}
	}
	return &raw.InstanceFlexibilityPolicy{
		InstanceSelections: selections,
	}
}

// discoverInstanceZone finds the zone GCP placed our just-created
// instance in by name. Uses AggregatedList scoped to the project with
// a server-side name filter, so the response is bounded to (at most)
// one entry per zone matching our instance name.
//
// AggregatedList returns the instance via per-zone scopes; we extract
// the zone from the instance's Zone self-link (".../zones/<zone>")
// rather than relying on the response's scope key to avoid coupling
// to the map's structure.
func (c *ComputeUtil) discoverInstanceZone() (string, error) {
	resp, err := c.service.Instances.AggregatedList(c.project).
		Filter(fmt.Sprintf("name eq %q", c.instanceName)).
		Do()
	if err != nil {
		return "", fmt.Errorf("aggregatedList lookup for %q: %w", c.instanceName, err)
	}

	for _, scope := range resp.Items {
		for _, inst := range scope.Instances {
			if inst.Name != c.instanceName {
				// Defensive: filter is server-side, but a name eq filter
				// can match by prefix in some legacy API responses.
				continue
			}
			zone, err := zoneFromInstanceURL(inst.Zone)
			if err != nil {
				return "", fmt.Errorf("parsing zone from instance %q self-link: %w", c.instanceName, err)
			}
			return zone, nil
		}
	}
	return "", fmt.Errorf("instance %q not found in any zone after bulkInsert (operation completed but aggregatedList did not return it)", c.instanceName)
}

// finishPostCreate runs the create-time work that's identical between
// the direct path and bulkInsert: look the instance up by name, sync
// the discovered placement back into Driver state so libmachine
// persists it, add the firewall tag, push the SSH public key.
func (c *ComputeUtil) finishPostCreate(d *Driver) error {
	instance, err := c.instance()
	if err != nil {
		return fmt.Errorf("looking up just-created instance %q: %w", c.instanceName, err)
	}

	// bulkInsert (and the flex policy in particular) owns machine-type
	// and zone placement; the operator's --google-machine-type / --google-zone
	// flags only seeded the policy. Write the actual values back to
	// Driver so they get persisted to config.json and metric labels
	// downstream (gitlab-runner!6740's target_machine_type / target_zone)
	// reflect reality rather than the operator's input.
	c.syncDriverStateFromInstance(d, instance)

	if err := c.addFirewallTag(instance); err != nil {
		return fmt.Errorf("adding firewall tag to bulkInsert instance %q: %w", c.instanceName, err)
	}

	return c.uploadSSHKey(instance, d.GetSSHKeyPath())
}

// syncDriverStateFromInstance populates Driver.ResolvedZone and
// Driver.ResolvedMachineType from a just-created instance. Called
// after a successful create so the resolved values get persisted by
// libmachine alongside the operator-intent Zone / MachineType fields.
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

// zoneFromInstanceURL parses the zone segment from a compute self-link
// of the form ".../zones/<zone>/instances/<name>". Parses with net/url
// first so genuinely-malformed input fails fast rather than slipping
// past substring matching, then pulls the zone via a tight regex on
// the path.
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

// machineTypeFromInstanceURL parses the machine-type segment from a
// compute self-link of the form ".../machineTypes/<type>".
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

// warnAboutIgnoredFlagsInBulkInsertMode walks the driver flags that
// bulkInsert ignores or overrides and logs a single warning per
// non-default value. Makes migration friction visible: operators
// copying a working direct-create config into bulkInsert mode see
// exactly which flags now have different semantics.
func (c *ComputeUtil) warnAboutIgnoredFlagsInBulkInsertMode(d *Driver) {
	ignored := map[string]bool{
		// --google-zone is overridden by the location policy; operator
		// intent in this flag is silently ignored unless it happens to
		// be one of the LocationZones.
		"google-zone": d.Zone != "" && d.Zone != defaultZone,
		// --google-machine-type is overridden by the flex policy when
		// one is set; without flex it's used as the single shape.
		"google-machine-type": len(d.FlexMachineTypes) > 0 && d.MachineType != "" && d.MachineType != defaultMachineType,
		// --google-address (static IP) doesn't survive zone selection
		// at create time — pinning to a specific NIC conflicts with
		// bulkInsert picking a zone post-hoc.
		"google-address": d.Address != "",
		// --google-accelerator (GPU/TPU) requires a zone-specific
		// resource URL; we can't construct it before placement.
		"google-accelerator": d.Accelerator != "",
	}
	for flag, set := range ignored {
		if set {
			log.Warnf("Flag --%s is ignored or overridden in bulkInsert mode (--google-region is set)", flag)
		}
	}
}


