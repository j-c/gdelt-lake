// Example parameters. Copy this file (e.g. to my.bicepparam, which .gitignore keeps out of the repo), replace the
// placeholders, then follow README.md.
using './main.bicep'

param image = 'ghcr.io/<owner>/gdelt-lake:<version>'

param vnetResourceId = '/subscriptions/<subscription-id>/resourceGroups/<network-rg>/providers/Microsoft.Network/virtualNetworks/<vnet>'
param jobsSubnetPrefix = '<free /27, e.g. 10.0.8.0/27>'
param privateEndpointSubnetName = '<existing subnet for private endpoints>'
param blobPrivateDnsZoneId = '/subscriptions/<subscription-id>/resourceGroups/<dns-rg>/providers/Microsoft.Network/privateDnsZones/privatelink.blob.core.windows.net'

param alertEmail = '<you@example.com>'

// History and retention. Deletion must be at least the backfill window.
param backfill = '182d'
param deleteAfterDays = 730

// Tiering by age since a file was written: off by default (everything stays Hot).
param coolAfterDays = 0
param coldAfterDays = 0

// Who may read the lake (Entra ID object IDs of users, groups or service principals).
param readerPrincipalIds = []
