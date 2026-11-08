// gdelt-lake on Azure: the reference deployment (design A11, A17, A22, A23).
//
// Creates a resource group (gdelt-lake by default) holding:
//   - a storage account for the lake (private endpoint only, Entra ID auth only) with lifecycle rules
//   - a Container Apps environment in a subnet of an existing virtual network
//   - two scheduled jobs: the ingester every 15 minutes, and an hourly `status` check
//   - a managed identity that can write the lake container, and nothing else
//   - a Log Analytics workspace for the jobs' logs, and email alerts on failed job executions
//   - unless you name an existing one, a private DNS zone for the private endpoint, linked to the virtual network
// and, in the virtual network's resource group, the jobs' subnet (unless you bring your own).
//
// Deployed at subscription scope (az deployment sub create). See README.md in this folder.

targetScope = 'subscription'

// ---------------------------------------------------------------------------------------------------------------------
// Parameters

@description('Resource group for the lake; created if it doesn\'t exist.')
param resourceGroupName string = 'gdelt-lake'

@description('Azure region for the resource group and everything in it. Defaults to the deployment\'s location.')
param location string = deployment().location

@description('Prefix for resource names: lowercase letters and digits.')
@minLength(3)
@maxLength(11)
param name string = 'gdeltlake'

@description('The ingester image, pinned to a version, e.g. ghcr.io/<owner>/gdelt-lake:0.1.0.')
param image string

@description('Resource ID of the existing virtual network the jobs and the private endpoint go in.')
param vnetResourceId string

@description('Name of the subnet to create for the Container Apps environment (delegated to Microsoft.App/environments).')
param jobsSubnetName string = 'snet-gdelt-lake'

@description('Address prefix for that subnet: a free /27 or larger in the virtual network. Ignored when existingJobsSubnetId is set.')
param jobsSubnetPrefix string = ''

@description('Use an existing, already delegated subnet instead of creating one (for example when policy requires an NSG on every subnet).')
param existingJobsSubnetId string = ''

@description('Name of an existing subnet in the same virtual network for the storage account\'s private endpoint (not the jobs subnet).')
param privateEndpointSubnetName string

@description('Resource ID of an existing private DNS zone privatelink.blob.core.windows.net linked to the virtual network, for the private endpoint\'s record. Empty: the deployment creates that zone in its resource group and links it to the virtual network.')
param blobPrivateDnsZoneId string = ''

@description('Email address for alerts. Empty: no alerts.')
param alertEmail string = ''

@description('History to fetch (LAKE_BACKFILL): a duration such as 182d, or a date such as 2025-01-01. Empty: none.')
param backfill string = '182d'

@description('Most time a run spends on backfill (LAKE_BACKFILL_BUDGET). 0 pauses backfill.')
param backfillBudget string = '10m'

@description('Move data files to the Cool tier this many days after they were written. 0: never.')
@minValue(0)
param coolAfterDays int = 0

@description('Move data files to the Cold tier this many days after they were written. 0: never. Must be more than coolAfterDays when both are set.')
@minValue(0)
param coldAfterDays int = 0

@description('Delete data files this many days after they were written. 0: never. Must be at least the backfill window, or status reports history that is gone.')
@minValue(0)
param deleteAfterDays int = 730

@description('Log Analytics daily ingestion cap, in GB (a string, e.g. "0.5"). "-1": no cap.')
param logDailyCapGb string = '0.5'

@description('Entra ID object IDs (users, groups or service principals) given read access to the lake.')
param readerPrincipalIds array = []

@description('Tags for every resource.')
param tags object = {}

// ---------------------------------------------------------------------------------------------------------------------
// Resource group and network

var vnetParts = split(vnetResourceId, '/')
var vnetSubscription = vnetParts[2]
var vnetResourceGroup = vnetParts[4]
var vnetName = vnetParts[8]

resource rg 'Microsoft.Resources/resourceGroups@2025-04-01' = {
  name: resourceGroupName
  location: location
  tags: tags
}

// The jobs' subnet, in the virtual network's own resource group.
module jobsSubnet 'modules/subnet.bicep' = if (empty(existingJobsSubnetId)) {
  name: '${name}-jobs-subnet'
  scope: resourceGroup(vnetSubscription, vnetResourceGroup)
  params: {
    vnetName: vnetName
    subnetName: jobsSubnetName
    addressPrefix: jobsSubnetPrefix
  }
}

// So that the jobs, inside Azure, resolve the storage account to its private endpoint.
module dns 'modules/dns.bicep' = if (empty(blobPrivateDnsZoneId)) {
  name: '${name}-dns'
  scope: rg
  params: {
    vnetResourceId: vnetResourceId
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// The lake

module lake 'modules/lake.bicep' = {
  name: '${name}-lake'
  scope: rg
  params: {
    location: location
    name: name
    image: image
    jobsSubnetId: empty(existingJobsSubnetId) ? jobsSubnet!.outputs.id : existingJobsSubnetId
    privateEndpointSubnetId: '${vnetResourceId}/subnets/${privateEndpointSubnetName}'
    blobPrivateDnsZoneId: empty(blobPrivateDnsZoneId) ? dns!.outputs.zoneId : blobPrivateDnsZoneId
    alertEmail: alertEmail
    backfill: backfill
    backfillBudget: backfillBudget
    coolAfterDays: coolAfterDays
    coldAfterDays: coldAfterDays
    deleteAfterDays: deleteAfterDays
    logDailyCapGb: logDailyCapGb
    readerPrincipalIds: readerPrincipalIds
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Outputs

output resourceGroup string = rg.name
output storageAccountName string = lake.outputs.storageAccountName
output blobEndpoint string = lake.outputs.blobEndpoint
output container string = lake.outputs.container
output ingestJob string = lake.outputs.ingestJob
output statusJob string = lake.outputs.statusJob
output identityPrincipalId string = lake.outputs.identityPrincipalId
@description('For an A record (<account>.blob.core.windows.net) in DNS servers outside Azure, e.g. on-premises.')
output privateEndpointIp string = lake.outputs.privateEndpointIp
