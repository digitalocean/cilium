// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ipam

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	. "github.com/cilium/checkmate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eniTypes "github.com/cilium/cilium/pkg/aws/eni/types"
	"github.com/cilium/cilium/pkg/checker"
	"github.com/cilium/cilium/pkg/cidr"
	"github.com/cilium/cilium/pkg/datapath/fake"
	ipamOption "github.com/cilium/cilium/pkg/ipam/option"
	ipamTypes "github.com/cilium/cilium/pkg/ipam/types"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/trigger"
)

func TestIPNotAvailableInPoolError(t *testing.T) {
	err := NewIPNotAvailableInPoolError(net.ParseIP("1.1.1.1"))
	err2 := NewIPNotAvailableInPoolError(net.ParseIP("1.1.1.1"))
	assert.Equal(t, err, err2)
	assert.True(t, errors.Is(err, err2))

	err = NewIPNotAvailableInPoolError(net.ParseIP("2.1.1.1"))
	err2 = NewIPNotAvailableInPoolError(net.ParseIP("1.1.1.1"))
	assert.NotEqual(t, err, err2)
	assert.False(t, errors.Is(err, err2))

	err = NewIPNotAvailableInPoolError(net.ParseIP("2.1.1.1"))
	err2 = errors.New("another error")
	assert.NotEqual(t, err, err2)
	assert.False(t, errors.Is(err, err2))

	err = errors.New("another error")
	err2 = NewIPNotAvailableInPoolError(net.ParseIP("2.1.1.1"))
	assert.NotEqual(t, err, err2)
	assert.False(t, errors.Is(err, err2))

	err = NewIPNotAvailableInPoolError(net.ParseIP("1.1.1.1"))
	err2 = nil
	assert.False(t, errors.Is(err, err2))

	err = nil
	err2 = NewIPNotAvailableInPoolError(net.ParseIP("1.1.1.1"))
	assert.False(t, errors.Is(err, err2))

	// We don't match against strings. It must be the sentinel value.
	err = errors.New("IP 2.1.1.1 is not available")
	err2 = NewIPNotAvailableInPoolError(net.ParseIP("2.1.1.1"))
	assert.NotEqual(t, err, err2)
	assert.False(t, errors.Is(err, err2))
}

type testConfigurationCRD struct{}

func (t *testConfigurationCRD) IPv4Enabled() bool                        { return true }
func (t *testConfigurationCRD) IPv6Enabled() bool                        { return false }
func (t *testConfigurationCRD) HealthCheckingEnabled() bool              { return true }
func (t *testConfigurationCRD) UnreachableRoutesEnabled() bool           { return false }
func (t *testConfigurationCRD) IPAMMode() string                         { return ipamOption.IPAMCRD }
func (t *testConfigurationCRD) SetIPv4NativeRoutingCIDR(cidr *cidr.CIDR) {}
func (t *testConfigurationCRD) GetIPv4NativeRoutingCIDR() *cidr.CIDR     { return nil }
func (t *testConfigurationCRD) IPv4NativeRoutingCIDR() *cidr.CIDR        { return nil }

func newFakeNodeStore(conf Configuration, c *C) *nodeStore {
	t, err := trigger.NewTrigger(trigger.Parameters{
		Name:        "fake-crd-allocator-node-refresher",
		MinInterval: 3 * time.Second,
		TriggerFunc: func(reasons []string) {},
	})
	if err != nil {
		log.WithError(err).Fatal("Unable to initialize CiliumNode synchronization trigger")
	}
	store := &nodeStore{
		allocators:         []*crdAllocator{},
		allocationPoolSize: map[Family]int{},
		conf:               conf,
		refreshTrigger:     t,
	}
	return store
}

func (s *IPAMSuite) TestMarkForReleaseNoAllocate(c *C) {
	cn := newCiliumNode("node1", 4, 4, 0)
	dummyResource := ipamTypes.AllocationIP{Resource: "foo"}
	for i := 1; i <= 4; i++ {
		cn.Spec.IPAM.Pool[fmt.Sprintf("1.1.1.%d", i)] = dummyResource
	}

	fakeAddressing := fake.NewNodeAddressing()
	conf := &testConfigurationCRD{}
	initNodeStore.Do(func() {
		sharedNodeStore = newFakeNodeStore(conf, c)
		sharedNodeStore.ownNode = cn
	})
	ipam := NewIPAM(fakeAddressing, conf, &ownerMock{}, &ownerMock{}, &resourceMock{}, &mtuMock, nil)
	sharedNodeStore.updateLocalNodeResource(cn)

	// Allocate the first 3 IPs
	for i := 1; i <= 3; i++ {
		epipv4 := netip.MustParseAddr(fmt.Sprintf("1.1.1.%d", i))
		_, err := ipam.IPv4Allocator.Allocate(epipv4.AsSlice(), fmt.Sprintf("test%d", i), PoolDefault())
		c.Assert(err, IsNil)
	}

	// Update 1.1.1.4 as marked for release like operator would.
	cn.Status.IPAM.ReleaseIPs["1.1.1.4"] = ipamOption.IPAMMarkForRelease
	// Attempts to allocate 1.1.1.4 should fail, since it's already marked for release
	epipv4 := netip.MustParseAddr("1.1.1.4")
	_, err := ipam.IPv4Allocator.Allocate(epipv4.AsSlice(), "test", PoolDefault())
	c.Assert(err, NotNil)
	// Call agent's CRD update function. status for 1.1.1.4 should change from marked for release to ready for release
	sharedNodeStore.updateLocalNodeResource(cn)
	c.Assert(string(cn.Status.IPAM.ReleaseIPs["1.1.1.4"]), checker.Equals, ipamOption.IPAMReadyForRelease)

	// Verify that 1.1.1.3 is denied for release, since it's already in use
	cn.Status.IPAM.ReleaseIPs["1.1.1.3"] = ipamOption.IPAMMarkForRelease
	sharedNodeStore.updateLocalNodeResource(cn)
	c.Assert(string(cn.Status.IPAM.ReleaseIPs["1.1.1.3"]), checker.Equals, ipamOption.IPAMDoNotRelease)
}

func Test_validateENIConfig(t *testing.T) {
	type args struct {
		node *ciliumv2.CiliumNode
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		want    string
	}{
		{
			name: "Consistent ENI config",
			args: args{
				node: &ciliumv2.CiliumNode{
					Spec: ciliumv2.NodeSpec{
						IPAM: ipamTypes.IPAMSpec{
							Pool: ipamTypes.AllocationMap{
								"10.1.1.226": ipamTypes.AllocationIP{
									Resource: "eni-1",
								},
							},
						},
					},
					Status: ciliumv2.NodeStatus{
						ENI: eniTypes.ENIStatus{
							ENIs: map[string]eniTypes.ENI{
								"eni-1": {
									ID: "eni-1",
									Addresses: []string{
										"10.1.1.226",
										"10.1.1.229",
									},
									VPC: eniTypes.AwsVPC{
										ID:          "vpc-1",
										PrimaryCIDR: "10.1.0.0/16",
										CIDRs: []string{
											"10.1.0.0/16",
											"10.2.0.0/16",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "Missing VPC Primary CIDR",
			args: args{
				node: &ciliumv2.CiliumNode{
					Spec: ciliumv2.NodeSpec{
						IPAM: ipamTypes.IPAMSpec{
							Pool: ipamTypes.AllocationMap{
								"10.1.1.226": ipamTypes.AllocationIP{
									Resource: "eni-1",
								},
							},
						},
					},
					Status: ciliumv2.NodeStatus{
						ENI: eniTypes.ENIStatus{
							ENIs: map[string]eniTypes.ENI{
								"eni-1": {
									ID: "eni-1",
									Addresses: []string{
										"10.1.1.226",
										"10.1.1.229",
									},
									VPC: eniTypes.AwsVPC{
										ID: "vpc-1",
									},
								},
							},
						},
					},
				},
			},
			wantErr: true,
			want:    "VPC Primary CIDR not set for ENI eni-1",
		},
		{
			name: "VPC CIDRs contain invalid value",
			args: args{
				node: &ciliumv2.CiliumNode{
					Spec: ciliumv2.NodeSpec{
						IPAM: ipamTypes.IPAMSpec{
							Pool: ipamTypes.AllocationMap{
								"10.1.1.226": ipamTypes.AllocationIP{
									Resource: "eni-1",
								},
							},
						},
					},
					Status: ciliumv2.NodeStatus{
						ENI: eniTypes.ENIStatus{
							ENIs: map[string]eniTypes.ENI{
								"eni-1": {
									ID: "eni-1",
									Addresses: []string{
										"10.1.1.226",
										"10.1.1.229",
									},
									VPC: eniTypes.AwsVPC{
										ID:          "vpc-1",
										PrimaryCIDR: "10.1.0.0/16",
										CIDRs: []string{
											"",
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: true,
			want:    "VPC CIDR not set for ENI eni-1",
		},
		{
			name: "ENI not found in status",
			args: args{
				node: &ciliumv2.CiliumNode{
					Spec: ciliumv2.NodeSpec{
						IPAM: ipamTypes.IPAMSpec{
							Pool: ipamTypes.AllocationMap{
								"10.1.1.226": ipamTypes.AllocationIP{
									Resource: "eni-1",
								},
							},
						},
					},
					Status: ciliumv2.NodeStatus{
						ENI: eniTypes.ENIStatus{
							ENIs: map[string]eniTypes.ENI{
								"eni-2": {
									ID: "eni-2",
									Addresses: []string{
										"10.1.1.226",
										"10.1.1.229",
									},
									VPC: eniTypes.AwsVPC{
										ID:          "vpc-1",
										PrimaryCIDR: "10.1.0.0/16",
										CIDRs: []string{
											"10.1.0.0/16",
											"10.2.0.0/16",
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: true,
			want:    "ENI eni-1 not found in status",
		},
		{
			name: "ENI IP not found in status",
			args: args{
				node: &ciliumv2.CiliumNode{
					Spec: ciliumv2.NodeSpec{
						IPAM: ipamTypes.IPAMSpec{
							Pool: ipamTypes.AllocationMap{
								"10.1.1.227": ipamTypes.AllocationIP{
									Resource: "eni-1",
								},
							},
						},
					},
					Status: ciliumv2.NodeStatus{
						ENI: eniTypes.ENIStatus{
							ENIs: map[string]eniTypes.ENI{
								"eni-1": {
									ID: "eni-1",
									Addresses: []string{
										"10.1.1.226",
										"10.1.1.229",
									},
									VPC: eniTypes.AwsVPC{
										ID:          "vpc-1",
										PrimaryCIDR: "10.1.0.0/16",
										CIDRs: []string{
											"10.1.0.0/16",
											"10.2.0.0/16",
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: true,
			want:    "ENI eni-1 does not have address 10.1.1.227",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateENIConfig(tt.args.node)
			require.Equal(t, tt.wantErr, got != nil, "error: %v", got)
			if tt.wantErr {
				require.Equal(t, tt.want, got.Error())
			}
		})
	}
}
