// Example parameters for deploy/shoal-explore-web/azure/main.bicep.
//
// Every value here is an OBVIOUS PLACEHOLDER. Copy this file, replace the
// placeholders with your environment's real values, and deploy with:
//
//   az deployment group create \
//     --resource-group <your-rg> \
//     --template-file main.bicep \
//     --parameters main.bicepparam
//
// No secrets belong in this file. The container needs no client secret (the
// OIDC authenticator validates inbound tokens only), and the Azure Files key
// is resolved at deploy time by the template's listKeys() call.

using './main.bicep'

param location = 'eastus'
param namePrefix = 'shoal-explore'

// Replace with your registry/repository:tag. Use a pinned tag, never :latest,
// so a single-writer redeploy is a deliberate, reviewable change.
param containerImage = 'REGISTRY-PLACEHOLDER.azurecr.io/shoal-explore-web:REPLACE-WITH-PINNED-TAG'

param containerPort = 8098
param useAcrManagedIdentity = true
param planSkuName = 'P1v3'

// Replace these generic OIDC placeholders with values from your provider.
param oidcIssuer = 'https://identity.example.test'
param oidcAudience = 'shoal-api'
param oidcAuthorizationClaim = 'access'
param oidcReaderValues = 'reader'
param oidcContributorValues = 'contributor'

// Leave empty to derive metadata and signing keys from the issuer.
param oidcDiscoveryUrl = ''
param oidcJwksUri = ''

// Leave both empty for API-only bearer authentication. To enable browser login,
// set a public-client identifier and provider-approved scopes.
param oidcBrowserClientId = ''
param oidcBrowserScope = ''

// Existing parameter files using entraTenantId/entraClientId and related
// entra* names continue to compile against main.bicep. Those names are
// deprecated compatibility aliases; prefer the oidc* parameters above.

// Host-authority allow-list (PR #295's -allowed-host / SHOAL_ALLOWED_HOST).
// EMPTY (the default) makes the template use the App Service default hostname,
// so the built-in *.azurewebsites.net endpoint works with no extra config.
// To serve a custom domain, list it here (comma-separated), e.g.:
//   param allowedHosts = 'explorer.example.test,myapp.azurewebsites.net'
// Use the BARE hostname (no :port) for names reached over HTTPS on 443.
param allowedHosts = ''

param stateShareQuotaGiB = 100
param storageSkuName = 'Standard_LRS'
