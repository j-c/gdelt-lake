# Azure deployment

The reference deployment: the ingester as a scheduled [Container Apps job](https://learn.microsoft.com/azure/container-apps/jobs),
writing to a storage account that's reachable only from your private network. Defined in Bicep
([`main.bicep`](main.bicep)); deployed with the Azure CLI.

## What it creates

A resource group (`resourceGroupName`, `gdelt-lake` by default; created if it doesn't exist) holding:

- **Storage account** for the lake: general-purpose v2, Standard, locally redundant (LRS), Hot tier. No public network
  access and no account keys: everything signs in with Entra ID. Container `gdelt`.
- **Private endpoint** for the account's blob service, in an existing subnet of your virtual network.
- **Private DNS zone** `privatelink.blob.core.windows.net`, linked to the virtual network, holding the endpoint's record,
  so the jobs (inside Azure) reach the account privately. If you already have such a zone linked to the network, pass
  its ID as `blobPrivateDnsZoneId` instead (a network can't be linked to two zones of the same name). The link falls
  back to public DNS for names the zone doesn't hold, so other storage accounts' names resolve as before.
- **Lifecycle rules** on the data folders (`events/`, `mentions/`, `gkg/`; never `_state/`), by time since a file was
  written: deletion after `deleteAfterDays` (730 by default), and optional moves to Cool (`coolAfterDays`) and Cold
  (`coldAfterDays`), both off by default.
- **Container Apps environment** (Consumption, internal: no public endpoint) in a subnet of your virtual network,
  delegated to `Microsoft.App/environments`: one the deployment creates in the virtual network's own resource group
  (`jobsSubnetPrefix`), or your own (`existingJobsSubnetId`).
- **Two scheduled jobs**, both running the published image:
  - `<name>-ingest`: `gdelt-lake run` every 15 minutes (UTC), 1 vCPU and 2 GiB, stopped after 14 minutes so runs
    never overlap;
  - `<name>-status`: `gdelt-lake status` every hour; it fails when the lake has gaps, falls behind, or the last run
    failed.
- **Managed identity** for the jobs, allowed to read and write the lake container and nothing else.
- **Log Analytics workspace** for the jobs' logs: 30-day retention, a daily cap (`logDailyCapGb`, 0.5 GB by default).
- **Email alerts** (if `alertEmail` is set): one when an ingest run fails, one when the status check fails.

Readers you list in `readerPrincipalIds` get read access to the lake.

## Before you start

- The Azure CLI (2.53 or later, for `.bicepparam` files) and `az login` to the tenant.
- Rights: the deployment runs at subscription scope and creates the resource group, so Owner on the subscription (or
  Contributor plus Role Based Access Control Administrator, since it assigns roles); permission to create a subnet in
  the virtual network and to join its private endpoint subnet (Network Contributor on the network's resource group
  covers both); and, if you pass an existing DNS zone, Private DNS Zone Contributor on it.
- The resource providers `Microsoft.App`, `Microsoft.OperationalInsights` and `Microsoft.Insights` registered in the
  subscription (`az provider register --namespace Microsoft.App`, and so on).
- From your network: a free address range of at least /27 for the jobs (or your own delegated subnet), and the name
  of an existing subnet for private endpoints.
- The jobs need outbound HTTPS to `data.gdeltproject.org` (GDELT) and `ghcr.io` (the image). If the virtual network
  sends internet traffic through a firewall, allow those.
- If the virtual network itself is defined in a template that lists its subnets, add the jobs subnet there too, or a
  later deployment of that template may remove it. Or create the subnet yourself (delegated to
  `Microsoft.App/environments`) and pass `existingJobsSubnetId` instead of `jobsSubnetPrefix`.

## Deploy

1. Copy the example parameters and fill them in. `*.bicepparam` files other than the example are ignored by git.

   ```
   cp main.example.bicepparam my.bicepparam
   ```

2. Preview the deployment. Nothing changes yet. `--location` is the region the data lives in (and where Azure keeps
   the deployment's record).

   ```
   az deployment sub what-if --location <region> --name gdelt-lake --parameters my.bicepparam
   ```

   Expect the resource group and about 17 resources in it to be created (plus the jobs subnet in the network's resource
   group, unless you brought your own), and nothing modified or deleted.

3. Deploy.

   ```
   az deployment sub create --location <region> --name gdelt-lake --parameters my.bicepparam
   ```

4. **Machines outside Azure** (e.g. on-premises consumers) resolve the account with your own DNS: add an A record for
   `<storage account name>.blob.core.windows.net` pointing at the private endpoint's IP, the same way as for any other
   private storage account. Both are in the deployment's outputs:

   ```
   az deployment sub show --name gdelt-lake --query "properties.outputs.{account:storageAccountName.value, ip:privateEndpointIp.value}"
   ```

## Check it

(The commands use the default names: resource group `gdelt-lake`, jobs `gdeltlake-ingest` and `gdeltlake-status`.)

- **Runs.** Within 15 minutes of the deployment, the ingest job has an execution, and it succeeds:

  ```
  az containerapp job execution list --name gdeltlake-ingest --resource-group gdelt-lake --output table
  ```

  New role assignments can take a few minutes to apply, so the very first run may fail with an authorisation error;
  the next one should succeed.

- **Health.** Start the status job instead of waiting for the hour, then check its execution succeeded:

  ```
  az containerapp job start --name gdeltlake-status --resource-group gdelt-lake
  az containerapp job execution list --name gdeltlake-status --resource-group gdelt-lake --output table
  ```

  While backfill is still running, status reports it as progress, not as a problem.

- **Logs.** In the Log Analytics workspace:

  ```
  ContainerAppConsoleLogs_CL
  | where ContainerJobName_s == "gdeltlake-ingest"
  | project TimeGenerated, Log_s
  | order by TimeGenerated desc
  ```

## Read the lake

From a machine on your private network (so the account name resolves to the private endpoint), signed in with
`az login` as one of the readers:

```sql
INSTALL azure; LOAD azure;
CREATE SECRET lake (TYPE azure, PROVIDER credential_chain, ACCOUNT_NAME '<storage account name>');

SELECT date, translation.source_language AS language, page_title, document_identifier
FROM read_parquet('az://gdelt/gkg/date=*/*.parquet', hive_partitioning = true, union_by_name = true)
WHERE date >= DATE '2026-10-31'
  AND list_contains(list_transform(themes, lambda t: t.theme), 'ARMEDCONFLICT')
LIMIT 10;
```

The storage account name is in the deployment's outputs (step 4 of Deploy).

Reading from outside Azure is charged as outbound data transfer after the first 100 GB a month. If your consumers
scan a lot, sync a recent window to a local disk and query that instead (`azcopy sync`).

## Cost

Fixed costs come from the network, not the jobs:

- **The environment's managed resources.** A Container Apps environment in your own virtual network gets a Standard
  load balancer and a static public IP for outbound traffic, in its `ME_…` resource group, billed at their usual
  rates (list prices about US$0.025 an hour and US$0.005 an hour: about US$22 a month together).
- **The storage account's private endpoint** (about US$7 a month, plus a small charge per GB through it).
- **Storage** for the lake: about 70 GB a year of data at Hot prices, less with the lifecycle rules.

The jobs normally finish in under a minute, which at this schedule stays within, or close to, the Container Apps
monthly free allowance; backfill runs use their full budget until history is complete. Log volume is small. Check the
actual figures in Cost Management after the first week.

## Change or remove it

Re-running the deployment with changed parameters updates it in place (for example a new image version, or
different lifecycle rules). To remove it, delete the resource group (this also removes the DNS zone it created), then
the jobs subnet if the deployment created it:

```
az group delete --name gdelt-lake
az network vnet subnet delete --resource-group <network-rg> --vnet-name <vnet> --name snet-gdelt-lake
```
