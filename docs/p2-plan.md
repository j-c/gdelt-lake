# P2 plan: Azure

Status: **in progress.** Steps 1, 2, 4 and 6 are done; deployed by the owner about 12 Nov and ingesting since (step 5). Left: step 3's DuckDB query against Azurite and step 5's query from the private network (hermes-02 reaches the private endpoint since 19 Nov; it needs a reader identity). Implements phase P2 of [design.md](design.md): the Azure Blob adapter and the Bicep recipe,
with storage reachable only from private networks (R13). Done when the deployment has run a week in Azure with no
intervention, with gaps recorded and the actual monthly cost known.

| Step | What | Checked by |
|------|------|------------|
| 1 | **Azure Blob adapter** (`LAKE_STORAGE=azure`; managed identity, `az login` or a connection string). | The storage contract tests against Azurite in CI; a real ingest run into Azurite. Done. |
| 2 | **Image publishing** to GitHub Container Registry (`publish.yml`: `:main` on every push, versions on `v*` tags). | The first publish run. Done. |
| 3 | **Bicep recipe** ([deploy/azure](../deploy/azure/README.md), design A22, A23): storage account with private endpoint and lifecycle rules, Container Apps environment in the deployer's network, the ingest and status jobs, managed identity, Log Analytics, alerts. | Bicep build and linter in CI; the README's DuckDB query over `az://` against Azurite. Deployment itself is checked in step 5. |
| 4 | **Public release**: the repository and image made public; tag `v0.1.0`. | A pre-release review (history, licence, no private details); an anonymous pull of the image. Done: public, `v0.1.0` tagged and published as `ghcr.io/j-c/gdelt-lake:0.1.0`, which pulls anonymously (checked 19 Nov). |
| 5 | **Deploy** with the recipe: `what-if`, then create; first runs, status, alerts and a query from the private network. | The checks in deploy/azure/README.md. Deployed by the owner about 12 Nov and ingesting since (owner, 19 Nov); the query from the private network is not checked yet. |
| 6 | **A week in Azure** with no intervention. | Gaps recorded by `status`; cost from Cost Management. Done: every hourly check from 14 Nov 19:05 to 18 Nov 22:05 UTC succeeded (the last 100 executions), with no intervention; cost for the first week US$9 (Cost Management, the lake's resource group and its `ME_` group; owner, 19 Nov), about US$39 a month at that rate. The first week includes the 182-day backfill's job time. |

## Later

- **Fixed cost of the Container Apps environment** (owner, 19 Nov: leave as is, revisit later). About US$22 of the
  US$39 a month is the load balancer and outbound IP that Container Apps adds to any environment in the deployer's
  network. The alternative found: run the ingester on an always-on on-prem Docker host writing over the VPN to the
  private endpoint (a service principal instead of the managed identity), leaving the private endpoint and storage at
  about US$10 a month; it depends on that host, and the Azure alerts move with it. Another Azure compute service would
  need a NAT gateway for outbound traffic, which costs more.
