package entrypoint_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datawire/ambassador/v2/cmd/ambex"
	"github.com/datawire/ambassador/v2/cmd/entrypoint"
	v3bootstrap "github.com/datawire/ambassador/v2/pkg/api/envoy/config/bootstrap/v3"
	v3cluster "github.com/datawire/ambassador/v2/pkg/api/envoy/config/cluster/v3"
	"github.com/datawire/ambassador/v2/pkg/kates"
	"github.com/datawire/ambassador/v2/pkg/snapshot/v1"
	"github.com/datawire/dlib/dlog"
)

// The IR layer produces a set of "feature" data describing things in use in the system.
// For this test, we only care about the count of invalid secrets.
type FakeFeatures struct {
	Invalid map[string]int `json:"invalid_counts"`
}

// The Fake struct is a test harness for edgestack. It spins up the key portions of the edgestack
// control plane that contain the bulk of its business logic, but instead of requiring tests to feed
// the business logic inputs via a real kubernetes or real consul deployment, inputs can be fed
// directly into the business logic via the harness APIs. This is not only several orders of
// magnitute faster, this also provides the author of the test perfect control over the ordering of
// events.
//
// By default the Fake struct only invokes the first part of the pipeline that forms the control
// plane. If you use the EnvoyConfig option you can run the rest of the control plane. There is also
// a Timeout option that controls how long the harness waits for the desired Snapshot and/or
// EnvoyConfig to come along.
//
// Note that this test depends on diagd being in your path. If diagd is not available, the test will
// be skipped.

// TestFakeHello is a basic "Hello, world!" style of test in that it demonstrates some things about
// the Fake test harness, but it does actually do valid testing of the system as well.
func TestFakeHello(t *testing.T) {
	// You can use os.Setenv to set environment variables, and they will affect the test harness.
	// Here, we'll force secret validation, so that invalid Secrets won't get passed all the way
	// to Envoy.
	os.Setenv("AMBASSADOR_FORCE_SECRET_VALIDATION", "true")

	// Use RunFake() to spin up the ambassador control plane with its inputs wired up to the Fake
	// APIs. This will automatically invoke the Setup() method for the Fake and also register the
	// Teardown() method with the Cleanup() hook of the supplied testing.T object.
	//
	// Note that we _must_ set EnvoyConfig true to allow checking IR Features later, even though
	// we don't actually do any checking of the Envoy config in this test.
	f := entrypoint.RunFake(t, entrypoint.FakeConfig{EnvoyConfig: true}, nil)

	// The Fake harness has a store for both kubernetes resources and consul endpoint data. We can
	// use the UpsertFile() to method to load as many resources as we would like. This is much like
	// doing a `kubectl apply` to a real kubernetes API server, however apply uses fancy merge
	// logic, whereas UpsertFile() does a simple Upsert operation. The `testdata/FakeHello.yaml`
	// file has a single mapping named "hello".
	assert.NoError(t, f.UpsertFile("testdata/FakeHello.yaml"))

	// Note, also, that we needn't limit ourselves to a single input file. Here, we'll upsert
	// a second file containing a broken TLS certificate, and we'll trust autoflush to do the
	// right thing for us.
	assert.NoError(t, f.UpsertFile("testdata/BrokenSecret.yaml"))

	// Initially the Fake harness is paused. This means we can make as many method calls as we want
	// to in order to set up our initial conditions, and no inputs will be fed into the control
	// plane. To feed inputs to the control plane, we can choose to either manually invoke the
	// Flush() method whenever we want to send the control plane inputs, or for convenience we can
	// enable AutoFlush so that inputs are set whenever we modify data that the control plane is
	// watching.
	//
	// XXX Well... that's the way it should work. For now, though, we need to manually flush a
	// single time for both files, because something else strange is going on.
	f.Flush()
	// f.AutoFlush(true)

	// Once the control plane has started processing inputs, we need some way to observe its
	// computation. The Fake harness provides two ways to do this. The GetSnapshot() method allows
	// us to observe the snapshots assembled by the watchers for further processing. The
	// GetEnvoyConfig() method allows us to observe the envoy configuration produced from a
	// snapshot. Both these methods take a predicate so the can search for a snapshot that satisifes
	// whatever conditions are being tested. This allows the test to verify that the correct
	// computation is occurring without being overly prescriptive about the exact number of
	// snapshots and/or envoy configs that are produce to achieve a certain result.
	snap, err := f.GetSnapshot(func(snap *snapshot.Snapshot) bool {
		hasMappings := len(snap.Kubernetes.Mappings) > 0
		hasSecrets := len(snap.Kubernetes.Secrets) > 0
		hasInvalid := len(snap.Invalid) > 0

		return hasMappings && hasSecrets && hasInvalid
	})
	require.NoError(t, err)

	// Check that the snapshot contains the mapping from the file.
	assert.Equal(t, "hello", snap.Kubernetes.Mappings[0].Name)

	// This snapshot also needs to have one good secret...
	assert.Equal(t, 1, len(snap.Kubernetes.Secrets))
	assert.Equal(t, "tls-cert", snap.Kubernetes.Secrets[0].Name)

	// ...and one invalid secret.
	assert.Equal(t, 1, len(snap.Invalid))
	assert.Equal(t, "tls-broken-cert", snap.Invalid[0].GetName())

	// Finally, we need to see a single invalid Secret in the IR Features.
	var features FakeFeatures
	err = f.GetFeatures(dlog.NewTestContext(t, false), &features)
	require.NoError(t, err)

	assert.Equal(t, 1, features.Invalid["Secret"])
}

// TestFakeHelloNoSecretValidation will cover the exact same paths as TestFakeHello, but with
// the AMBASSADOR_FORCE_SECRET_VALIDATION environment variable disabled. We expect the number
// of secrets to be different.
func TestFakeHelloNoSecretValidation(t *testing.T) {
	// There are many fewer comments here, because this function so closely mirrors
	// TestFakeHello. Read the comments there!
	//
	// We explicitly force secret validation off, so that broken secrets will not get dropped.
	// They will still appear in the Invalid list, and in the IR Features invalid_counts dict.
	os.Setenv("AMBASSADOR_FORCE_SECRET_VALIDATION", "false")

	// Note that we _must_ set EnvoyConfig true to allow checking IR Features later.
	f := entrypoint.RunFake(t, entrypoint.FakeConfig{EnvoyConfig: true}, nil)

	// Once again, we'll use both FakeHello.yaml and BrokenSecret.yaml... and once again, we'll
	// manually flush a single time.
	assert.NoError(t, f.UpsertFile("testdata/FakeHello.yaml"))
	assert.NoError(t, f.UpsertFile("testdata/BrokenSecret.yaml"))
	f.Flush()

	// We'll use the same predicate as TestFakeHello to grab a snapshot with some mappings,
	// some secrets, and some invalid objects.
	snap, err := f.GetSnapshot(func(snap *snapshot.Snapshot) bool {
		hasMappings := len(snap.Kubernetes.Mappings) > 0
		hasSecrets := len(snap.Kubernetes.Secrets) > 0
		hasInvalid := len(snap.Invalid) > 0

		return hasMappings && hasSecrets && hasInvalid
	})
	require.NoError(t, err)

	// This snapshot needs to have the correct Mapping...
	assert.Equal(t, "hello", snap.Kubernetes.Mappings[0].Name)

	// ...but it'll claim to have two good Secrets (even though one is really broken).
	assert.Equal(t, 2, len(snap.Kubernetes.Secrets))
	secretNames := []string{snap.Kubernetes.Secrets[0].Name, snap.Kubernetes.Secrets[1].Name}
	assert.Contains(t, secretNames, "tls-broken-cert")
	assert.Contains(t, secretNames, "tls-cert")

	// Even though the broken cert is in our "valid" list above, it should stil show
	// up in the Invalid objects list...
	assert.Equal(t, 1, len(snap.Invalid))
	assert.Equal(t, "tls-broken-cert", snap.Invalid[0].GetName())

	// ...and in our invalid_counts in the IR Features.
	var features FakeFeatures
	err = f.GetFeatures(dlog.NewTestContext(t, false), &features)
	require.NoError(t, err)

	assert.Equal(t, 1, features.Invalid["Secret"])
}

// TestFakeHelloEC will cover mTLS Secret validation with EC (Elliptic Curve) Private Keys. Once again,
// it closely mirrors TestFakeHello (with secret validation on), just using different secrets.
func TestFakeHelloEC(t *testing.T) {
	// There are many fewer comments here, because this function so closely mirrors
	// TestFakeHello. Read the comments there!
	//
	// Make sure secret validation is on (so broken secrets won't show up in the "good" list).
	os.Setenv("AMBASSADOR_FORCE_SECRET_VALIDATION", "true")

	// Note that we _must_ set EnvoyConfig true to allow checking IR Features later.
	f := entrypoint.RunFake(t, entrypoint.FakeConfig{EnvoyConfig: true}, nil)

	// AutoFlush will work fine here, with just the one file.
	f.AutoFlush(true)

	// FakeHelloEC.yaml contains good secrets and invalid secrets, so we just need the one file.
	assert.NoError(t, f.UpsertFile("testdata/FakeHelloEC.yaml"))

	// Once again, we can use the same predicate as TestFakeHello to grab a snapshot with some mappings,
	// some secrets, and some invalid objects.
	snap, err := f.GetSnapshot(func(snap *snapshot.Snapshot) bool {
		hasMappings := len(snap.Kubernetes.Mappings) > 0
		hasSecrets := len(snap.Kubernetes.Secrets) > 0
		hasInvalid := len(snap.Invalid) > 0

		return hasMappings && hasSecrets && hasInvalid
	})
	require.NoError(t, err)

	// This snapshot needs to have the correct Mapping...
	assert.Equal(t, "hello-elliptic-curve", snap.Kubernetes.Mappings[0].Name)

	// ...and it also needs two good Secrets. Note that neither of these is the broken one...
	assert.Equal(t, 2, len(snap.Kubernetes.Secrets))
	secretNames := []string{snap.Kubernetes.Secrets[0].Name, snap.Kubernetes.Secrets[1].Name}
	assert.Contains(t, secretNames, "hello-elliptic-curve-client")
	assert.Contains(t, secretNames, "tls-cert")

	// ...since the broken cert shows up only in the invalid list (and the IR Features, of
	// course).
	assert.Equal(t, 1, len(snap.Invalid))
	assert.Equal(t, "hello-elliptic-curve-broken-server", snap.Invalid[0].GetName())

	var features FakeFeatures
	err = f.GetFeatures(dlog.NewTestContext(t, false), &features)
	require.NoError(t, err)

	assert.Equal(t, 1, features.Invalid["Secret"])
}

// TestFakeHelloWithEnvoyConfig is a Hello-World style test that also checks the Envoy configuration.
func TestFakeHelloWithEnvoyConfig(t *testing.T) {
	// Use the FakeConfig parameter to conigure the Fake harness. In this case we want to inspect
	// the EnvoyConfig that is produced from the inputs we feed the control plane.
	f := entrypoint.RunFake(t, entrypoint.FakeConfig{EnvoyConfig: true}, nil)

	// We will use the same inputs we used in TestFakeHello. A single mapping named "hello".
	assert.NoError(t, f.UpsertFile("testdata/FakeHello.yaml"))
	// Instead of using AutoFlush(true) we will manually Flush() when we want to feed inputs to the
	// control plane.
	f.Flush()

	// Grab the next snapshot that has mappings. The v3bootstrap logic should actually gaurantee this
	// is also the first mapping, but we aren't trying to test that here.
	snap, err := f.GetSnapshot(func(snap *snapshot.Snapshot) bool {
		return len(snap.Kubernetes.Mappings) > 0
	})
	require.NoError(t, err)
	// The first snapshot should contain the one and only mapping we have supplied the control
	// plane.x
	assert.Equal(t, "hello", snap.Kubernetes.Mappings[0].Name)

	// Create a predicate that will recognize the cluster we care about. The surjection from
	// Mappings to clusters is a bit opaque, so we just look for a cluster that contains the name
	// hello.
	isHelloCluster := func(c *v3cluster.Cluster) bool {
		return strings.Contains(c.Name, "hello")
	}

	// Grab the next envoy config that satisfies our predicate.
	envoyConfig, err := f.GetEnvoyConfig(func(envoy *v3bootstrap.Bootstrap) bool {
		return FindCluster(envoy, isHelloCluster) != nil
	})
	require.NoError(t, err)

	// Now let's dig into the envoy configuration and check that the correct target endpoint is
	// present.
	//
	// Note: This is admittedly quite verbose as envoy configuration is very dense. I expect we will
	// introduce an API that will provide a more abstract and convenient way of navigating envoy
	// configuration, however that will be covered in a future PR. The core of that logic is already
	// developing inside ambex since ambex slices and dices the envoy config in order to implement
	// RDS and enpdoint routing.
	cluster := FindCluster(envoyConfig, isHelloCluster)
	endpoints := cluster.LoadAssignment.Endpoints
	require.NotEmpty(t, endpoints)
	lbEndpoints := endpoints[0].LbEndpoints
	require.NotEmpty(t, lbEndpoints)
	endpoint := lbEndpoints[0].GetEndpoint()
	address := endpoint.Address.GetSocketAddress().Address
	assert.Equal(t, "hello", address)

	// Since there are no broken secrets here, we should have no entries in the IR Features.
	var features FakeFeatures
	err = f.GetFeatures(dlog.NewTestContext(t, false), &features)
	require.NoError(t, err)

	assert.Equal(t, 0, features.Invalid["Secret"])
}

func FindCluster(envoyConfig *v3bootstrap.Bootstrap, predicate func(*v3cluster.Cluster) bool) *v3cluster.Cluster {
	for _, cluster := range envoyConfig.StaticResources.Clusters {
		if predicate(cluster) {
			return cluster
		}
	}

	return nil
}

func deltaSummary(t *testing.T, snaps ...*snapshot.Snapshot) []string {
	summary := []string{}

	var typestr string

	for _, snap := range snaps {
		for _, delta := range snap.Deltas {
			switch delta.DeltaType {
			case kates.ObjectAdd:
				typestr = "add"
			case kates.ObjectUpdate:
				typestr = "update"
			case kates.ObjectDelete:
				typestr = "delete"
			default:
				// Bug because the programmer needs to add another case here.
				t.Fatalf("missing case for DeltaType enum: %#v", delta)
			}

			summary = append(summary, fmt.Sprintf("%s %s %s", typestr, delta.Kind, delta.Name))
		}
	}

	sort.Strings(summary)

	return summary
}

// getSnapshots is like f.GetSnapshot, but returns the list of every snapshot evaluated (rather than
// discarding the snapshots from before predicate returns true).  This is particularly important if
// you're looking at deltas; you don't want to discard any deltas just because two snapshots didn't
// get coalesced.
func getSnapshots(f *entrypoint.Fake, predicate func(*snapshot.Snapshot) bool) ([]*snapshot.Snapshot, error) {
	var ret []*snapshot.Snapshot
	for {
		snap, err := f.GetSnapshot(func(_ *snapshot.Snapshot) bool {
			return true
		})
		if err != nil {
			return nil, err
		}
		ret = append(ret, snap)
		if predicate(snap) {
			break
		}
	}
	return ret, nil
}

// This test will cover how to exercise the consul portion of the control plane. In principal it is
// the same as supplying kubernetes resources, however it uses the ConsulEndpoint() method to
// provide consul data.
func TestFakeHelloConsul(t *testing.T) {
	os.Setenv("CONSULPORT", "8500")
	os.Setenv("CONSULHOST", "consul-1")

	// Create our Fake harness and tell it to produce envoy configuration.
	f := entrypoint.RunFake(t, entrypoint.FakeConfig{EnvoyConfig: true}, nil)

	// Feed the control plane the kubernetes resources supplied in the referenced file. In this case
	// that includes a consul resolver and a mapping that uses that consul resolver.
	assert.NoError(t, f.UpsertFile("testdata/FakeHelloConsul.yaml"))
	// This test is a bit more interesting for the control plane from a v3bootstrapping perspective,
	// so we invoke Flush() manually rather than using AutoFlush(true). The control plane needs to
	// figure out that there is a mapping that depends on consul endpoint data, and it needs to wait
	// until that data is available before producing the first snapshot.
	f.Flush()

	// In prior tests we have only examined the snapshots that were ready to be processed, but the
	// watcher doesn't process every snapshot it constructs, it can discard various snapshots for
	// different reasons. Using GetSnapshotEntry() we can pull entries from the full log of
	// snapshots considered as opposed to just the skipping straight to the ones that are ready to
	// be processed.
	//
	// In this case the snapshot is considered incomplete until we supply enough consul endpoint
	// data for edgestack to construct an envoy config that won't send requests to our hello mapping
	// into a black hole.
	entry, err := f.GetSnapshotEntry(func(entry entrypoint.SnapshotEntry) bool {
		return entry.Disposition == entrypoint.SnapshotIncomplete && len(entry.Snapshot.Kubernetes.Mappings) > 0
	})
	require.NoError(t, err)
	// Check that the snapshot contains the mapping from the file.
	assert.Equal(t, "hello", entry.Snapshot.Kubernetes.Mappings[0].Name)
	// ..and the TCPMapping as well
	assert.Equal(t, "hello-tcp", entry.Snapshot.Kubernetes.TCPMappings[0].Name)

	// Now let's supply the endpoint data for the hello service referenced by our hello mapping.
	f.ConsulEndpoint("dc1", "hello", "1.2.3.4", 8080)
	// And also supply the endpoint data for the hello-tcp service referenced by our hello mapping.
	f.ConsulEndpoint("dc1", "hello-tcp", "5.6.7.8", 3099)
	f.Flush()

	// The Fake harness also tracks endpoints that get sent to ambex. We can use the GetEndpoints()
	// method to access them and check to see that the endpoint we supplied got delivered to ambex.
	endpoints, err := f.GetEndpoints(func(endpoints *ambex.Endpoints) bool {
		_, ok := endpoints.Entries["consul/dc1/hello"]
		if ok {
			_, okTcp := endpoints.Entries["consul/dc1/hello-tcp"]
			return okTcp
		}
		return false
	})
	require.NoError(t, err)
	assert.Len(t, endpoints.Entries, 2)
	assert.Equal(t, "1.2.3.4", endpoints.Entries["consul/dc1/hello"][0].Ip)

	// Grab the next snapshot that has both mappings, tcpmappings, and a Consul resolver. The v3bootstrap logic
	// should actually guarantee this is also the first mapping, but we aren't trying to test
	// that here.
	snap, err := f.GetSnapshot(func(snap *snapshot.Snapshot) bool {
		return (len(snap.Kubernetes.Mappings) > 0) && (len(snap.Kubernetes.TCPMappings) > 0) && (len(snap.Kubernetes.ConsulResolvers) > 0)
	})
	require.NoError(t, err)
	// The first snapshot should contain both the mapping and tcpmapping we have supplied the control
	// plane.
	assert.Equal(t, "hello", snap.Kubernetes.Mappings[0].Name)
	assert.Equal(t, "hello-tcp", snap.Kubernetes.TCPMappings[0].Name)

	// It should also contain one ConsulResolver with a Spec.Address of
	// "consul-server.default:8500" (where the 8500 came from an environment variable).
	assert.Equal(t, "consul-server.default:8500", snap.Kubernetes.ConsulResolvers[0].Spec.Address)

	// Check that our deltas are what we expect.
	assert.Equal(t, []string{"add ConsulResolver consul-dc1", "add Mapping hello", "add TCPMapping hello-tcp"}, deltaSummary(t, snap))

	// Create a predicate that will recognize the cluster we care about. The surjection from
	// Mappings to clusters is a bit opaque, so we just look for a cluster that contains the name
	// hello.
	isHelloTCPCluster := func(c *v3cluster.Cluster) bool {
		return strings.Contains(c.Name, "hello_tcp")
	}
	isHelloCluster := func(c *v3cluster.Cluster) bool {
		return strings.Contains(c.Name, "hello") && !isHelloTCPCluster(c)
	}

	// Grab the next envoy config that satisfies our predicate.
	envoyConfig, err := f.GetEnvoyConfig(func(envoy *v3bootstrap.Bootstrap) bool {
		return FindCluster(envoy, isHelloCluster) != nil
	})
	require.NoError(t, err)

	// Now let's check that the cluster produced properly references the endpoints that have already
	// arrived at ambex.
	cluster := FindCluster(envoyConfig, isHelloCluster)
	assert.NotNil(t, cluster)
	// It uses the consul resolver, so it should not embed the load assignment directly.
	assert.Nil(t, cluster.LoadAssignment)
	// It *should* have an EdsConfig.
	edsConfig := cluster.GetEdsClusterConfig()
	require.NotNil(t, edsConfig)
	// The EdsConfig *should* reference an endpoint.
	eps := endpoints.Entries[edsConfig.ServiceName]
	require.Len(t, eps, 1)
	// The endpoint it references *should* have our supplied ip address.
	assert.Equal(t, "1.2.3.4", eps[0].Ip)

	// Finally, let's check that the TCP cluster is OK too.
	cluster = FindCluster(envoyConfig, isHelloTCPCluster)
	assert.NotNil(t, cluster)
	// It uses the consul resolver, so it should not embed the load assignment directly.
	assert.Nil(t, cluster.LoadAssignment)
	// It *should* have an EdsConfig.
	edsConfig = cluster.GetEdsClusterConfig()
	require.NotNil(t, edsConfig)
	// The EdsConfig *should* reference an endpoint.
	eps = endpoints.Entries[edsConfig.ServiceName]
	require.Len(t, eps, 1)
	// The endpoint it references *should* have our supplied ip address.
	assert.Equal(t, "5.6.7.8", eps[0].Ip)

	// Next up, change the Consul resolver definition.
	assert.NoError(t, f.UpsertYAML(`
---
apiVersion: getambassador.io/v3alpha1
kind: ConsulResolver
metadata:
  name: consul-dc1
spec:
  address: $CONSULHOST:$CONSULPORT
  datacenter: dc1
`))
	f.Flush()

	// Repeat the snapshot checks. We must have mappings and consulresolvers...
	snap, err = f.GetSnapshot(func(snap *snapshot.Snapshot) bool {
		return (len(snap.Kubernetes.Mappings) > 0) && (len(snap.Kubernetes.TCPMappings) > 0) && (len(snap.Kubernetes.ConsulResolvers) > 0)
	})
	require.NoError(t, err)

	// ...with one delta, namely the ConsulResolver...
	assert.Equal(t, []string{"update ConsulResolver consul-dc1"}, deltaSummary(t, snap))

	// ...where the mapping name hasn't changed...
	assert.Equal(t, "hello", snap.Kubernetes.Mappings[0].Name)
	assert.Equal(t, "hello-tcp", snap.Kubernetes.TCPMappings[0].Name)

	// ...but the Consul server address has.
	assert.Equal(t, "consul-1:8500", snap.Kubernetes.ConsulResolvers[0].Spec.Address)

	// Finally, delete the Consul resolver, then replace it. This is mostly just testing that
	// things don't crash.

	assert.NoError(t, f.Delete("ConsulResolver", "default", "consul-dc1"))
	f.Flush()

	assert.NoError(t, f.UpsertYAML(`
---
apiVersion: getambassador.io/v3alpha1
kind: ConsulResolver
metadata:
  name: consul-dc1
spec:
  address: $CONSULHOST:9999
  datacenter: dc1
`))
	f.Flush()

	// Repeat all the checks.
	snaps, err := getSnapshots(f, func(snap *snapshot.Snapshot) bool {
		return (len(snap.Kubernetes.Mappings) > 0) && (len(snap.Kubernetes.TCPMappings) > 0) && (len(snap.Kubernetes.ConsulResolvers) > 0)
	})
	require.NoError(t, err)
	require.Greater(t, len(snaps), 0)
	snap = snaps[len(snaps)-1]

	// Two deltas here since we've deleted and re-added without a check in between.
	// (They appear out of order here because of string sorting. Don't panic.)
	assert.Equal(t, []string{"add ConsulResolver consul-dc1", "delete ConsulResolver consul-dc1"}, deltaSummary(t, snaps...))

	// ...one mapping...
	assert.Equal(t, "hello", snap.Kubernetes.Mappings[0].Name)
	assert.Equal(t, "hello-tcp", snap.Kubernetes.TCPMappings[0].Name)

	// ...and one ConsulResolver.
	assert.Equal(t, "consul-1:9999", snap.Kubernetes.ConsulResolvers[0].Spec.Address)
}
