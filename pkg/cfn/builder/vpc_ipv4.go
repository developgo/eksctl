package builder

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/pkg/errors"
	gfncfn "github.com/weaveworks/goformation/v4/cloudformation/cloudformation"
	gfnec2 "github.com/weaveworks/goformation/v4/cloudformation/ec2"
	gfnt "github.com/weaveworks/goformation/v4/cloudformation/types"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/outputs"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

const (
	cfnControlPlaneSGResource         = "ControlPlaneSecurityGroup"
	cfnSharedNodeSGResource           = "ClusterSharedNodeSecurityGroup"
	cfnIngressClusterToNodeSGResource = "IngressDefaultClusterToNodeSG"
)

// A IPv4VPCResourceSet builds the resources required for the specified VPC
type IPv4VPCResourceSet struct {
	rs            *resourceSet
	clusterConfig *api.ClusterConfig
	ec2API        ec2iface.EC2API
	vpcID         *gfnt.Value
	subnetDetails *SubnetDetails
}

type SubnetResource struct {
	Subnet           *gfnt.Value
	RouteTable       *gfnt.Value
	AvailabilityZone string
}

type SubnetDetails struct {
	Private []SubnetResource
	Public  []SubnetResource
}

// NewIPv4VPCResourceSet creates and returns a new VPCResourceSet
func NewIPv4VPCResourceSet(rs *resourceSet, clusterConfig *api.ClusterConfig, ec2API ec2iface.EC2API) *IPv4VPCResourceSet {
	var vpcRef *gfnt.Value
	if clusterConfig.VPC.ID == "" {
		vpcRef = rs.newResource("VPC", &gfnec2.VPC{
			CidrBlock:          gfnt.NewString(clusterConfig.VPC.CIDR.String()),
			EnableDnsSupport:   gfnt.True(),
			EnableDnsHostnames: gfnt.True(),
		})
	} else {
		vpcRef = gfnt.NewString(clusterConfig.VPC.ID)
	}

	return &IPv4VPCResourceSet{
		rs:            rs,
		clusterConfig: clusterConfig,
		ec2API:        ec2API,
		vpcID:         vpcRef,
		subnetDetails: &SubnetDetails{},
	}
}

func (v *IPv4VPCResourceSet) CreateTemplate() (*gfnt.Value, *SubnetDetails, error) {
	err := v.addResources()
	if err != nil {
		return nil, nil, err
	}
	v.addOutputs()
	return v.vpcID, v.subnetDetails, nil
}

// AddResources adds all required resources
func (v *IPv4VPCResourceSet) addResources() error {
	vpc := v.clusterConfig.VPC
	if vpc.ID != "" { // custom VPC has been set
		if err := v.importResources(); err != nil {
			return errors.Wrap(err, "error importing VPC resources")
		}
		return nil
	}

	if api.IsEnabled(vpc.AutoAllocateIPv6) {
		v.rs.newResource("AutoAllocatedCIDRv6", &gfnec2.VPCCidrBlock{
			VpcId:                       v.vpcID,
			AmazonProvidedIpv6CidrBlock: gfnt.True(),
		})
	}

	if v.isFullyPrivate() {
		v.noNAT()
		v.subnetDetails.Private = v.addSubnets(nil, api.SubnetTopologyPrivate, vpc.Subnets.Private)
		return nil
	}

	refIG := v.rs.newResource("InternetGateway", &gfnec2.InternetGateway{})
	vpcGA := "VPCGatewayAttachment"
	v.rs.newResource(vpcGA, &gfnec2.VPCGatewayAttachment{
		InternetGatewayId: refIG,
		VpcId:             v.vpcID,
	})

	refPublicRT := v.rs.newResource("PublicRouteTable", &gfnec2.RouteTable{
		VpcId: v.vpcID,
	})

	v.rs.newResource("PublicSubnetRoute", &gfnec2.Route{
		RouteTableId:               refPublicRT,
		DestinationCidrBlock:       gfnt.NewString(InternetCIDR),
		GatewayId:                  refIG,
		AWSCloudFormationDependsOn: []string{vpcGA},
	})

	v.subnetDetails.Public = v.addSubnets(refPublicRT, api.SubnetTopologyPublic, vpc.Subnets.Public)

	if err := v.addNATGateways(); err != nil {
		return err
	}

	v.subnetDetails.Private = v.addSubnets(nil, api.SubnetTopologyPrivate, vpc.Subnets.Private)
	return nil
}

func (s *SubnetDetails) PublicSubnetRefs() []*gfnt.Value {
	var subnetRefs []*gfnt.Value
	for _, subnetAZ := range s.Public {
		subnetRefs = append(subnetRefs, subnetAZ.Subnet)
	}
	return subnetRefs
}

func (s *SubnetDetails) PrivateSubnetRefs() []*gfnt.Value {
	var subnetRefs []*gfnt.Value
	for _, subnetAZ := range s.Private {
		subnetRefs = append(subnetRefs, subnetAZ.Subnet)
	}
	return subnetRefs
}

// addOutputs adds VPC resource outputs
func (v *IPv4VPCResourceSet) addOutputs() {
	v.rs.defineOutput(outputs.ClusterVPC, v.vpcID, true, func(val string) error {
		v.clusterConfig.VPC.ID = val
		return nil
	})
	if v.clusterConfig.VPC.NAT != nil {
		v.rs.defineOutputWithoutCollector(outputs.ClusterFeatureNATMode, v.clusterConfig.VPC.NAT.Gateway, false)
	}

	addSubnetOutput := func(subnetRefs []*gfnt.Value, topology api.SubnetTopology, outputName string) {
		v.rs.defineJoinedOutput(outputName, subnetRefs, true, func(value string) error {
			return vpc.ImportSubnetsFromIDList(v.ec2API, v.clusterConfig, topology, strings.Split(value, ","))
		})
	}

	if subnetAZs := v.subnetDetails.PrivateSubnetRefs(); len(subnetAZs) > 0 {
		addSubnetOutput(subnetAZs, api.SubnetTopologyPrivate, outputs.ClusterSubnetsPrivate)
	}

	if subnetAZs := v.subnetDetails.PublicSubnetRefs(); len(subnetAZs) > 0 {
		addSubnetOutput(subnetAZs, api.SubnetTopologyPublic, outputs.ClusterSubnetsPublic)
	}

	if v.isFullyPrivate() {
		v.rs.defineOutputWithoutCollector(outputs.ClusterFullyPrivate, true, true)
	}
}

// RenderJSON returns the rendered JSON
func (v *IPv4VPCResourceSet) RenderJSON() ([]byte, error) {
	return v.rs.renderJSON()
}

func (v *IPv4VPCResourceSet) addSubnets(refRT *gfnt.Value, topology api.SubnetTopology, subnets map[string]api.AZSubnetSpec) []SubnetResource {
	var subnetIndexForIPv6 int
	if api.IsEnabled(v.clusterConfig.VPC.AutoAllocateIPv6) {
		// this is same kind of indexing we have in vpc.SetSubnets
		switch topology {
		case api.SubnetTopologyPrivate:
			subnetIndexForIPv6 = len(v.clusterConfig.AvailabilityZones)
		case api.SubnetTopologyPublic:
			subnetIndexForIPv6 = 0
		}
	}

	var subnetResources []SubnetResource

	for name, subnet := range subnets {
		az := subnet.AZ
		nameAlias := strings.ToUpper(strings.Join(strings.Split(name, "-"), ""))
		subnet := &gfnec2.Subnet{
			AvailabilityZone: gfnt.NewString(az),
			CidrBlock:        gfnt.NewString(subnet.CIDR.String()),
			VpcId:            v.vpcID,
		}

		switch topology {
		case api.SubnetTopologyPrivate:
			// Choose the appropriate route table for private subnets
			refRT = gfnt.MakeRef("PrivateRouteTable" + nameAlias)
			subnet.Tags = []gfncfn.Tag{{
				Key:   gfnt.NewString("kubernetes.io/role/internal-elb"),
				Value: gfnt.NewString("1"),
			}}
		case api.SubnetTopologyPublic:
			subnet.Tags = []gfncfn.Tag{{
				Key:   gfnt.NewString("kubernetes.io/role/elb"),
				Value: gfnt.NewString("1"),
			}}
			subnet.MapPublicIpOnLaunch = gfnt.True()
		}
		subnetAlias := string(topology) + nameAlias
		refSubnet := v.rs.newResource("Subnet"+subnetAlias, subnet)
		v.rs.newResource("RouteTableAssociation"+subnetAlias, &gfnec2.SubnetRouteTableAssociation{
			SubnetId:     refSubnet,
			RouteTableId: refRT,
		})

		if api.IsEnabled(v.clusterConfig.VPC.AutoAllocateIPv6) {
			refSubnetSlices := getSubnetIPv6CIDRBlock((len(v.clusterConfig.AvailabilityZones) * 2) + 2)
			v.rs.newResource(subnetAlias+"CIDRv6", &gfnec2.SubnetCidrBlock{
				SubnetId:      refSubnet,
				Ipv6CidrBlock: gfnt.MakeFnSelect(gfnt.NewInteger(subnetIndexForIPv6), refSubnetSlices),
			})
			subnetIndexForIPv6++
		}

		subnetResources = append(subnetResources, SubnetResource{
			AvailabilityZone: az,
			RouteTable:       refRT,
			Subnet:           refSubnet,
		})
	}
	return subnetResources
}

func (v *IPv4VPCResourceSet) addNATGateways() error {
	switch *v.clusterConfig.VPC.NAT.Gateway {
	case api.ClusterHighlyAvailableNAT:
		v.haNAT()
	case api.ClusterSingleNAT:
		v.singleNAT()
	case api.ClusterDisableNAT:
		v.noNAT()
	default:
		// TODO validate this before starting to add resources
		return fmt.Errorf("%s is not a valid NAT gateway mode", *v.clusterConfig.VPC.NAT.Gateway)
	}
	return nil
}

func (v *IPv4VPCResourceSet) importResources() error {
	if subnets := v.clusterConfig.VPC.Subnets.Private; subnets != nil {
		var (
			subnetRoutes map[string]string
			err          error
		)
		if v.isFullyPrivate() {
			subnetRoutes, err = importRouteTables(v.ec2API, v.clusterConfig.VPC.Subnets.Private)
			if err != nil {
				return err
			}
		}

		subnetResources, err := makeSubnetResources(subnets, subnetRoutes)
		if err != nil {
			return err
		}
		v.subnetDetails.Private = subnetResources
	}

	if subnets := v.clusterConfig.VPC.Subnets.Public; subnets != nil {
		subnetResources, err := makeSubnetResources(subnets, nil)
		if err != nil {
			return err
		}
		v.subnetDetails.Public = subnetResources
	}

	return nil
}

func makeSubnetResources(subnets map[string]api.AZSubnetSpec, subnetRoutes map[string]string) ([]SubnetResource, error) {
	subnetResources := make([]SubnetResource, len(subnets))
	i := 0
	for _, network := range subnets {
		az := network.AZ
		sr := SubnetResource{
			AvailabilityZone: az,
			Subnet:           gfnt.NewString(network.ID),
		}

		if subnetRoutes != nil {
			rt, ok := subnetRoutes[network.ID]
			if !ok {
				return nil, errors.Errorf("failed to find an explicit route table associated with subnet %q; "+
					"eksctl does not modify the main route table if a subnet is not associated with an explicit route table", network.ID)
			}
			sr.RouteTable = gfnt.NewString(rt)
		}
		subnetResources[i] = sr
		i++
	}
	return subnetResources, nil
}

func importRouteTables(ec2API ec2iface.EC2API, subnets map[string]api.AZSubnetSpec) (map[string]string, error) {
	var subnetIDs []string
	for _, subnet := range subnets {
		subnetIDs = append(subnetIDs, subnet.ID)
	}

	var routeTables []*ec2.RouteTable
	var nextToken *string

	for {
		output, err := ec2API.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
			Filters: []*ec2.Filter{
				{
					Name:   aws.String("association.subnet-id"),
					Values: aws.StringSlice(subnetIDs),
				},
			},
			NextToken: nextToken,
		})

		if err != nil {
			return nil, errors.Wrap(err, "error describing route tables")
		}

		routeTables = append(routeTables, output.RouteTables...)

		if nextToken = output.NextToken; nextToken == nil {
			break
		}
	}

	subnetRoutes := make(map[string]string)
	for _, rt := range routeTables {
		for _, rta := range rt.Associations {
			if rta.Main != nil && *rta.Main {
				return nil, errors.New("subnets must be associated with a non-main route table; eksctl does not modify the main route table")
			}
			subnetRoutes[*rta.SubnetId] = *rt.RouteTableId
		}
	}
	return subnetRoutes, nil
}

func (v *IPv4VPCResourceSet) isFullyPrivate() bool {
	return v.clusterConfig.PrivateCluster.Enabled
}

var (
	sgProtoTCP           = gfnt.NewString("tcp")
	sgSourceAnywhereIPv4 = gfnt.NewString("0.0.0.0/0")
	sgSourceAnywhereIPv6 = gfnt.NewString("::/0")

	sgPortZero    = gfnt.NewInteger(0)
	sgMinNodePort = gfnt.NewInteger(1025)
	sgMaxNodePort = gfnt.NewInteger(65535)

	sgPortHTTPS = gfnt.NewInteger(443)
	sgPortSSH   = gfnt.NewInteger(22)
)

type clusterSecurityGroup struct {
	ControlPlane      *gfnt.Value
	ClusterSharedNode *gfnt.Value
}

func (v *IPv4VPCResourceSet) haNAT() {
	for _, az := range v.clusterConfig.AvailabilityZones {
		alphanumericUpperAZ := formatAZ(az)

		// Allocate an EIP
		v.rs.newResource("NATIP"+alphanumericUpperAZ, &gfnec2.EIP{
			Domain: gfnt.NewString("vpc"),
		})
		// Allocate a NAT gateway in the public subnet
		refNG := v.rs.newResource("NATGateway"+alphanumericUpperAZ, &gfnec2.NatGateway{
			AllocationId: gfnt.MakeFnGetAttString("NATIP"+alphanumericUpperAZ, "AllocationId"),
			SubnetId:     gfnt.MakeRef("SubnetPublic" + alphanumericUpperAZ),
		})

		// Allocate a routing table for the private subnet
		refRT := v.rs.newResource("PrivateRouteTable"+alphanumericUpperAZ, &gfnec2.RouteTable{
			VpcId: v.vpcID,
		})
		// Create a route that sends Internet traffic through the NAT gateway
		v.rs.newResource("NATPrivateSubnetRoute"+alphanumericUpperAZ, &gfnec2.Route{
			RouteTableId:         refRT,
			DestinationCidrBlock: gfnt.NewString(InternetCIDR),
			NatGatewayId:         refNG,
		})
		// Associate the routing table with the subnet
		v.rs.newResource("RouteTableAssociationPrivate"+alphanumericUpperAZ, &gfnec2.SubnetRouteTableAssociation{
			SubnetId:     gfnt.MakeRef("SubnetPrivate" + alphanumericUpperAZ),
			RouteTableId: refRT,
		})
	}
}

func (v *IPv4VPCResourceSet) singleNAT() {
	sortedAZs := v.clusterConfig.AvailabilityZones
	firstUpperAZ := strings.ToUpper(strings.Join(strings.Split(sortedAZs[0], "-"), ""))

	v.rs.newResource("NATIP", &gfnec2.EIP{
		Domain: gfnt.NewString("vpc"),
	})
	refNG := v.rs.newResource("NATGateway", &gfnec2.NatGateway{
		AllocationId: gfnt.MakeFnGetAttString("NATIP", "AllocationId"),
		SubnetId:     gfnt.MakeRef("SubnetPublic" + firstUpperAZ),
	})

	for _, az := range v.clusterConfig.AvailabilityZones {
		alphanumericUpperAZ := strings.ToUpper(strings.Join(strings.Split(az, "-"), ""))

		refRT := v.rs.newResource("PrivateRouteTable"+alphanumericUpperAZ, &gfnec2.RouteTable{
			VpcId: v.vpcID,
		})

		v.rs.newResource("NATPrivateSubnetRoute"+alphanumericUpperAZ, &gfnec2.Route{
			RouteTableId:         refRT,
			DestinationCidrBlock: gfnt.NewString(InternetCIDR),
			NatGatewayId:         refNG,
		})
		v.rs.newResource("RouteTableAssociationPrivate"+alphanumericUpperAZ, &gfnec2.SubnetRouteTableAssociation{
			SubnetId:     gfnt.MakeRef("SubnetPrivate" + alphanumericUpperAZ),
			RouteTableId: refRT,
		})
	}
}

func (v *IPv4VPCResourceSet) noNAT() {
	for _, az := range v.clusterConfig.AvailabilityZones {
		alphanumericUpperAZ := strings.ToUpper(strings.Join(strings.Split(az, "-"), ""))

		refRT := v.rs.newResource("PrivateRouteTable"+alphanumericUpperAZ, &gfnec2.RouteTable{
			VpcId: v.vpcID,
		})
		v.rs.newResource("RouteTableAssociationPrivate"+alphanumericUpperAZ, &gfnec2.SubnetRouteTableAssociation{
			SubnetId:     gfnt.MakeRef("SubnetPrivate" + alphanumericUpperAZ),
			RouteTableId: refRT,
		})
	}
}