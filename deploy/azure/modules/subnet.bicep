// A subnet for the Container Apps environment, created in an existing virtual network. Deployed into the virtual
// network's resource group by main.bicep.

param vnetName string
param subnetName string

@description('A free /27 or larger.')
param addressPrefix string

resource vnet 'Microsoft.Network/virtualNetworks@2025-05-01' existing = {
  name: vnetName
}

resource subnet 'Microsoft.Network/virtualNetworks/subnets@2025-05-01' = {
  parent: vnet
  name: subnetName
  properties: {
    addressPrefix: addressPrefix
    delegations: [
      {
        name: 'container-apps'
        properties: {
          serviceName: 'Microsoft.App/environments'
        }
      }
    ]
  }
}

output id string = subnet.id
