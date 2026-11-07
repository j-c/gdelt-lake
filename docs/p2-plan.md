# P2 plan: Azure

Status: **in progress.** Implements phase P2 of [design.md](design.md): the Azure Blob adapter and the Bicep recipe,
with storage reachable only from private networks (R13). Done when the deployment has run a week in Azure with no
intervention, with gaps recorded and the actual monthly cost known.

| Step | What | Checked by |
|------|------|------------|
| 1 | **Azure Blob adapter** (`LAKE_STORAGE=azure`; managed identity, `az login` or a connection string). | The storage contract tests against Azurite in CI; a real ingest run into Azurite. Done. |
| 2 | **Image publishing** to GitHub Container Registry (`publish.yml`: `:main` on every push, versions on `v*` tags). | The first publish run. Done. |
| 3 | **Bicep recipe** ([deploy/azure](../deploy/azure/README.md), design A22, A23): storage account with private endpoint and lifecycle rules, Container Apps environment in the deployer's network, the ingest and status jobs, managed identity, Log Analytics, alerts. | Bicep build and linter in CI; the README's DuckDB query over `az://` against Azurite. Deployment itself is checked in step 5. |
| 4 | **Public release**: the repository and image made public; tag `v0.1.0`. | A pre-release review (history, licence, no private details); an anonymous pull of the image. |
| 5 | **Deploy** with the recipe: `what-if`, then create; first runs, status, alerts and a query from the private network. | The checks in deploy/azure/README.md. |
| 6 | **A week in Azure** with no intervention. | Gaps recorded by `status`; cost from Cost Management. |
