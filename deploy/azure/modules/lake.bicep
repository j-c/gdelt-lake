// The lake's resources, in its resource group: storage with private endpoint and lifecycle rules, the Container Apps
// environment and jobs, identity, logs and alerts. Deployed by main.bicep, which also creates the resource group and the
// network pieces; parameters are described there.

targetScope = 'resourceGroup'

param location string
param name string
param image string
param jobsSubnetId string
param privateEndpointSubnetId string
param blobPrivateDnsZoneId string
param alertEmail string
param backfill string
param backfillBudget string
param coolAfterDays int
param coldAfterDays int
param deleteAfterDays int
param logDailyCapGb string
param readerPrincipalIds array
param tags object

// ---------------------------------------------------------------------------------------------------------------------
// Names and constants

var containerName = 'gdelt'
var storageName = take('${name}${uniqueString(resourceGroup().id)}', 24)

// Built-in roles.
var blobDataContributor = 'ba92f5b4-2d11-453d-a403-e96b0029c9fe'
var blobDataReader = '2a2b9908-6ea1-4ae2-8e65-a410df84e7d1'

// ---------------------------------------------------------------------------------------------------------------------
// Storage: the lake

resource storage 'Microsoft.Storage/storageAccounts@2025-06-01' = {
  name: storageName
  location: location
  tags: tags
  kind: 'StorageV2'
  sku: {
    name: 'Standard_LRS' // anything lost can be fetched from GDELT again
  }
  properties: {
    accessTier: 'Hot'
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false // Entra ID only: the jobs' managed identity, and readers' own identities
    defaultToOAuthAuthentication: true
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    publicNetworkAccess: 'Disabled' // reachable only through the private endpoint (R13)
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'None'
    }
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2025-06-01' = {
  parent: storage
  name: 'default'
}

resource lakeContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2025-06-01' = {
  parent: blobService
  name: containerName
  properties: {
    publicAccess: 'None'
  }
}

// Lifecycle rules on the data folders only; _state/ is never moved or deleted (A23).
var lifecycleActions = union(
  coolAfterDays > 0 ? { tierToCool: { daysAfterModificationGreaterThan: coolAfterDays } } : {},
  coldAfterDays > 0 ? { tierToCold: { daysAfterModificationGreaterThan: coldAfterDays } } : {},
  deleteAfterDays > 0 ? { delete: { daysAfterModificationGreaterThan: deleteAfterDays } } : {}
)

resource lifecycle 'Microsoft.Storage/storageAccounts/managementPolicies@2025-06-01' = if (!empty(lifecycleActions)) {
  parent: storage
  name: 'default'
  properties: {
    policy: {
      rules: [
        {
          name: 'lake-data'
          enabled: true
          type: 'Lifecycle'
          definition: {
            filters: {
              blobTypes: ['blockBlob']
              prefixMatch: ['${containerName}/events/', '${containerName}/mentions/', '${containerName}/gkg/']
            }
            actions: {
              baseBlob: lifecycleActions
            }
          }
        }
      ]
    }
  }
}

resource privateEndpoint 'Microsoft.Network/privateEndpoints@2025-05-01' = {
  name: '${storageName}-blob'
  location: location
  tags: tags
  properties: {
    customNetworkInterfaceName: '${storageName}-blob-nic'
    subnet: {
      id: privateEndpointSubnetId
    }
    privateLinkServiceConnections: [
      {
        name: 'blob'
        properties: {
          privateLinkServiceId: storage.id
          groupIds: ['blob']
        }
      }
    ]
  }
}

// The endpoint's network interface, for its IP address (an output, for DNS servers outside Azure).
resource privateEndpointNic 'Microsoft.Network/networkInterfaces@2025-05-01' existing = {
  name: '${storageName}-blob-nic'
  dependsOn: [privateEndpoint]
}

resource privateDns 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2025-05-01' = {
  parent: privateEndpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: 'blob'
        properties: {
          privateDnsZoneId: blobPrivateDnsZoneId
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Identity and access

resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' = {
  name: '${name}-ingester'
  location: location
  tags: tags
}

resource writer 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(lakeContainer.id, identity.id, blobDataContributor)
  scope: lakeContainer
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', blobDataContributor)
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

resource readers 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for id in readerPrincipalIds: {
    name: guid(lakeContainer.id, id, blobDataReader)
    scope: lakeContainer
    properties: {
      roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', blobDataReader)
      principalId: id
    }
  }
]

// ---------------------------------------------------------------------------------------------------------------------
// Logs and the Container Apps environment

resource logs 'Microsoft.OperationalInsights/workspaces@2025-07-01' = {
  name: '${name}-logs'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
    workspaceCapping: {
      dailyQuotaGb: json(logDailyCapGb)
    }
  }
}

resource environment 'Microsoft.App/managedEnvironments@2025-07-01' = {
  name: '${name}-env'
  location: location
  tags: tags
  properties: {
    workloadProfiles: [
      {
        name: 'Consumption'
        workloadProfileType: 'Consumption'
      }
    ]
    vnetConfiguration: {
      infrastructureSubnetId: jobsSubnetId
      internal: true // no public endpoint; the jobs serve nothing
    }
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logs.properties.customerId
        sharedKey: logs.listKeys().primarySharedKey
      }
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Jobs

var lakeEnv = [
  { name: 'LAKE_STORAGE', value: 'azure' }
  { name: 'LAKE_AZURE_ACCOUNT_URL', value: storage.properties.primaryEndpoints.blob }
  { name: 'LAKE_AZURE_CONTAINER', value: containerName }
  { name: 'AZURE_CLIENT_ID', value: identity.properties.clientId } // picks the user-assigned identity
]

resource ingest 'Microsoft.App/jobs@2025-07-01' = {
  name: '${name}-ingest'
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    environmentId: environment.id
    workloadProfileName: 'Consumption'
    configuration: {
      triggerType: 'Schedule'
      scheduleTriggerConfig: {
        cronExpression: '*/15 * * * *' // UTC; GDELT publishes every 15 minutes
        parallelism: 1
        replicaCompletionCount: 1
      }
      // Under the 15-minute schedule, so runs never overlap: a run spends at most backfillBudget on history.
      replicaTimeout: 840
      replicaRetryLimit: 0 // the next run retries anything that failed
    }
    template: {
      containers: [
        {
          name: 'gdelt-lake'
          image: image
          args: ['run']
          resources: {
            cpu: json('1.0') // 1 vCPU also brings the temporary disk compaction needs (about 1 GB for a day of GKG)
            memory: '2Gi'
          }
          env: concat(lakeEnv, [
            { name: 'LAKE_BACKFILL', value: backfill }
            { name: 'LAKE_BACKFILL_BUDGET', value: backfillBudget }
            { name: 'LAKE_DUCKDB_MEMORY', value: '1GB' }
          ])
        }
      ]
    }
  }
  dependsOn: [writer, privateDns]
}

resource statusJob 'Microsoft.App/jobs@2025-07-01' = {
  name: '${name}-status'
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    environmentId: environment.id
    workloadProfileName: 'Consumption'
    configuration: {
      triggerType: 'Schedule'
      scheduleTriggerConfig: {
        cronExpression: '5 * * * *' // hourly; fails (and alerts) on data problems or a failed last run
        parallelism: 1
        replicaCompletionCount: 1
      }
      replicaTimeout: 300
      replicaRetryLimit: 0
    }
    template: {
      containers: [
        {
          name: 'gdelt-lake'
          image: image
          args: ['status']
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
          env: lakeEnv
        }
      ]
    }
  }
  dependsOn: [writer, privateDns]
}

// ---------------------------------------------------------------------------------------------------------------------
// Alerts: a failed execution of either job

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' = if (!empty(alertEmail)) {
  name: '${name}-alerts'
  location: 'global'
  tags: tags
  properties: {
    groupShortName: take(name, 12)
    enabled: true
    emailReceivers: [
      {
        name: 'email'
        emailAddress: alertEmail
        useCommonAlertSchema: true
      }
    ]
  }
}

var alerted = [
  { job: ingest.name, id: ingest.id, what: 'A gdelt-lake ingest run failed (exit 1: storage, DuckDB or another system error). See the job\'s logs.' }
  { job: statusJob.name, id: statusJob.id, what: 'gdelt-lake status reports a problem: gaps, the lake behind, files failing for over an hour, or a failed last run. Run the status job or read its logs.' }
]

resource failedAlerts 'Microsoft.Insights/metricAlerts@2026-01-01' = [
  for a in alerted: if (!empty(alertEmail)) {
    name: '${a.job}-failed'
    location: 'global'
    tags: tags
    properties: {
      description: a.what
      severity: 2
      enabled: true
      scopes: [a.id]
      evaluationFrequency: 'PT15M'
      windowSize: 'PT1H'
      autoMitigate: true
      criteria: {
        'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
        allOf: [
          {
            criterionType: 'StaticThresholdCriterion'
            name: 'failed-executions'
            metricNamespace: 'Microsoft.App/jobs'
            metricName: 'Executions'
            dimensions: [
              {
                name: 'state'
                operator: 'Include'
                values: ['Failed']
              }
            ]
            operator: 'GreaterThan'
            threshold: 0
            timeAggregation: 'Total'
          }
        ]
      }
      actions: [
        {
          actionGroupId: actionGroup.id
        }
      ]
    }
  }
]

// ---------------------------------------------------------------------------------------------------------------------
// Outputs

output storageAccountName string = storage.name
output blobEndpoint string = storage.properties.primaryEndpoints.blob
output container string = containerName
output ingestJob string = ingest.name
output statusJob string = statusJob.name
output identityPrincipalId string = identity.properties.principalId
@description('The private endpoint\'s IP address: for an A record in DNS servers outside Azure (e.g. on-premises).')
output privateEndpointIp string = privateEndpointNic.properties.ipConfigurations[0].properties.privateIPAddress
