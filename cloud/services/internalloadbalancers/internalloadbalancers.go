/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package internalloadbalancers

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2019-06-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/pkg/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1alpha3"
	azure "sigs.k8s.io/cluster-api-provider-azure/cloud"
)

// Spec specification for internal load balancer
type Spec struct {
	Name       string
	SubnetName string
	SubnetCidr string
	VnetName   string
	IPAddress  string
}

// Reconcile gets/creates/updates an internal load balancer.
func (s *Service) Reconcile(ctx context.Context, spec interface{}) error {
	internalLBSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("invalid internal load balancer specification")
	}
	s.Scope.V(2).Info("creating internal load balancer", "internal lb", internalLBSpec.Name)
	probeName := "HTTPSProbe"
	frontEndIPConfigName := "controlplane-internal-lbFrontEnd"
	backEndAddressPoolName := "controlplane-internal-backEndPool"
	idPrefix := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers", s.Scope.SubscriptionID(), s.Scope.ResourceGroup())
	lbName := internalLBSpec.Name
	var privateIP string

	internalLB, err := s.Client.Get(ctx, s.Scope.ResourceGroup(), internalLBSpec.Name)
	if err == nil {
		ipConfigs := internalLB.LoadBalancerPropertiesFormat.FrontendIPConfigurations
		if ipConfigs != nil && len(*ipConfigs) > 0 {
			privateIP = to.String((*ipConfigs)[0].FrontendIPConfigurationPropertiesFormat.PrivateIPAddress)
		}
	} else if azure.ResourceNotFound(err) {
		s.Scope.V(2).Info("internalLB not found in RG", "internal lb", internalLBSpec.Name, "resource group", s.Scope.ResourceGroup())
		privateIP, err = s.getAvailablePrivateIP(ctx, s.Scope.Vnet().ResourceGroup, internalLBSpec.VnetName, internalLBSpec.SubnetCidr, internalLBSpec.IPAddress)
		if err != nil {
			return err
		}
		s.Scope.V(2).Info("setting internal load balancer IP", "private ip", privateIP)
	} else {
		return errors.Wrap(err, "failed to look for existing internal LB")
	}

	s.Scope.V(2).Info("getting subnet", "subnet", internalLBSpec.SubnetName)
	subnet, err := s.SubnetsClient.Get(ctx, s.Scope.Vnet().ResourceGroup, internalLBSpec.VnetName, internalLBSpec.SubnetName)
	if err != nil {
		return errors.Wrap(err, "failed to get subnet")
	}

	s.Scope.V(2).Info("successfully got subnet", "subnet", internalLBSpec.SubnetName)

	// https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-standard-availability-zones#zone-redundant-by-default
	err = s.Client.CreateOrUpdate(ctx,
		s.Scope.ResourceGroup(),
		lbName,
		network.LoadBalancer{
			Sku:      &network.LoadBalancerSku{Name: network.LoadBalancerSkuNameStandard},
			Location: to.StringPtr(s.Scope.Location()),
			LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
				FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
					{
						Name: &frontEndIPConfigName,
						FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
							PrivateIPAllocationMethod: network.Static,
							Subnet:                    &subnet,
							PrivateIPAddress:          to.StringPtr(privateIP),
						},
					},
				},
				BackendAddressPools: &[]network.BackendAddressPool{
					{
						Name: &backEndAddressPoolName,
					},
				},
				Probes: &[]network.Probe{
					{
						Name: &probeName,
						ProbePropertiesFormat: &network.ProbePropertiesFormat{
							Protocol:          network.ProbeProtocolHTTPS,
							RequestPath:       to.StringPtr("/healthz"),
							Port:              to.Int32Ptr(s.Scope.APIServerPort()),
							IntervalInSeconds: to.Int32Ptr(15),
							NumberOfProbes:    to.Int32Ptr(4),
						},
					},
				},
				LoadBalancingRules: &[]network.LoadBalancingRule{
					{
						Name: to.StringPtr("LBRuleHTTPS"),
						LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
							Protocol:             network.TransportProtocolTCP,
							FrontendPort:         to.Int32Ptr(s.Scope.APIServerPort()),
							BackendPort:          to.Int32Ptr(s.Scope.APIServerPort()),
							IdleTimeoutInMinutes: to.Int32Ptr(4),
							EnableFloatingIP:     to.BoolPtr(false),
							LoadDistribution:     network.LoadDistributionDefault,
							FrontendIPConfiguration: &network.SubResource{
								ID: to.StringPtr(fmt.Sprintf("/%s/%s/frontendIPConfigurations/%s", idPrefix, lbName, frontEndIPConfigName)),
							},
							BackendAddressPool: &network.SubResource{
								ID: to.StringPtr(fmt.Sprintf("/%s/%s/backendAddressPools/%s", idPrefix, lbName, backEndAddressPoolName)),
							},
							Probe: &network.SubResource{
								ID: to.StringPtr(fmt.Sprintf("/%s/%s/probes/%s", idPrefix, lbName, probeName)),
							},
						},
					},
				},
			},
		})

	if err != nil {
		return errors.Wrap(err, "cannot create load balancer")
	}

	s.Scope.V(2).Info("successfully created internal load balancer", "internal lb", internalLBSpec.Name)
	return err
}

// Delete deletes the internal load balancer with the provided name.
func (s *Service) Delete(ctx context.Context, spec interface{}) error {
	internalLBSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("invalid internal load balancer specification")
	}
	s.Scope.V(2).Info("deleting internal load balancer", "internal lb", internalLBSpec.Name)
	err := s.Client.Delete(ctx, s.Scope.ResourceGroup(), internalLBSpec.Name)
	if err != nil && azure.ResourceNotFound(err) {
		// already deleted
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "failed to delete internal load balancer %s in resource group %s", internalLBSpec.Name, s.Scope.ResourceGroup())
	}

	s.Scope.V(2).Info("successfully deleted internal load balancer", "internal lb", internalLBSpec.Name)
	return nil
}

// getAvailablePrivateIP checks if the desired private IP address is available in a virtual network.
// If the IP address is taken or empty, it will make an attempt to find an available IP in the same subnet
func (s *Service) getAvailablePrivateIP(ctx context.Context, resourceGroup, vnetName, subnetCIDR, PreferredIPAddress string) (string, error) {
	ip := PreferredIPAddress
	if ip == "" {
		ip = azure.DefaultInternalLBIPAddress
		if subnetCIDR != infrav1.DefaultControlPlaneSubnetCIDR {
			// If the user provided a custom subnet CIDR without providing a private IP, try finding an available IP in the subnet space
			index := strings.LastIndex(subnetCIDR, ".")
			ip = subnetCIDR[0:(index+1)] + "0"
		}
	}
	result, err := s.VirtualNetworksClient.CheckIPAddressAvailability(ctx, resourceGroup, vnetName, ip)
	if err != nil {
		return "", errors.Wrap(err, "failed to check IP availability")
	}
	if !to.Bool(result.Available) {
		if len(to.StringSlice(result.AvailableIPAddresses)) == 0 {
			return "", errors.Errorf("IP %s is not available in vnet %s and there were no other available IPs found", ip, vnetName)
		}
		ip = to.StringSlice(result.AvailableIPAddresses)[0]
	}
	return ip, nil
}
