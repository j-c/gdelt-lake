// A private DNS zone for blob private endpoints, linked to the virtual network, so resources in that network (the jobs)
// resolve the storage account to its private endpoint. Used when the deployer has no such zone already.

param vnetResourceId string
param tags object

resource zone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: 'privatelink.blob.${environment().suffixes.storage}'
  location: 'global'
  tags: tags
}

resource link 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: zone
  name: 'lake-vnet'
  location: 'global'
  tags: tags
  properties: {
    virtualNetwork: {
      id: vnetResourceId
    }
    registrationEnabled: false
    // Names this zone has no record for (other storage accounts' private endpoints) still resolve as before, through
    // public DNS, instead of failing.
    resolutionPolicy: 'NxDomainRedirect'
  }
}

output zoneId string = zone.id
