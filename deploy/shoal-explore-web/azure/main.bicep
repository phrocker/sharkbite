// Single-instance Shoal Explorer workspace on Azure App Service for Containers.
//
// This template provisions the ONE-WRITER hosting shape chosen in
// deploy/shoal-explore-web/azure/README.md: a single Linux App Service instance
// running the shoal-explore-web container, with the state root (/var/lib/shoal)
// backed by a bring-your-own Azure Files share so the corpus and the durable
// policy catalog survive restarts as one mount.
//
// It contains NO secrets. The OIDC authenticator validates inbound bearer
// tokens and never accepts a client secret (verified in
// cmd/shoal-explore-web/main.go), so there is no application credential to store.
// The only credential is the Azure Files access key used to mount the share; it
// is resolved at deploy time with listKeys() and is never written to the
// repository. See the README for the Key Vault reference alternative.
//
// Validate structurally offline with:  az bicep build --file main.bicep

targetScope = 'resourceGroup'

@description('Azure region for all resources. Defaults to the resource group location.')
param location string = resourceGroup().location

@description('Lowercase prefix used to derive resource names. 2-17 chars, alphanumeric or dashes.')
@minLength(2)
@maxLength(17)
param namePrefix string = 'shoal-explore'

@description('Container image reference, including registry, repository and tag. Example: myregistry.azurecr.io/shoal-explore-web:1.0.0')
param containerImage string

@description('Port the container listens on. The app is started with -listen 0.0.0.0:<containerPort>; App Service forwards to it via WEBSITES_PORT.')
@minValue(1)
@maxValue(65535)
param containerPort int = 8098

@description('When true, App Service pulls the image from Azure Container Registry using the user-assigned managed identity created here. Grant it AcrPull separately (see README). When false, configure registry credentials out of band.')
param useAcrManagedIdentity bool = true

@description('App Service plan SKU name. Must be a plan that supports Always On and custom containers (for example P0v3, P1v3).')
param planSkuName string = 'P1v3'

@description('Exact OIDC token issuer.')
param oidcIssuer string = ''

@description('Comma-separated accepted OIDC token audiences.')
param oidcAudience string = ''

@description('Top-level token claim whose string values map to workspace authority.')
param oidcAuthorizationClaim string = ''

@description('Comma-separated authorization-claim values granted read-only workspace access.')
param oidcReaderValues string = 'reader'

@description('Comma-separated authorization-claim values granted read and ingest access.')
param oidcContributorValues string = 'contributor'

@description('Optional OIDC discovery URL override. Empty derives it from the issuer.')
param oidcDiscoveryUrl string = ''

@description('Optional JWKS URI override. Empty resolves via OIDC discovery.')
param oidcJwksUri string = ''

@description('Optional public-client identifier enabling browser Authorization Code + PKCE login.')
param oidcBrowserClientId string = ''

@description('Browser login scope. Required when oidcBrowserClientId is set.')
param oidcBrowserScope string = ''

@description('Deprecated compatibility parameter: former Entra tenant ID.')
param entraTenantId string = ''

@description('Deprecated compatibility parameter: former Entra client ID.')
param entraClientId string = ''

@description('Deprecated compatibility parameter: former reader app-role values.')
param entraReaderRoles string = 'Shoal.Reader'

@description('Deprecated compatibility parameter: former contributor app-role values.')
param entraContributorRoles string = 'Shoal.Contributor'

@description('Deprecated compatibility parameter: former exact issuer override.')
param entraIssuer string = ''

@description('Deprecated compatibility parameter: former JWKS URI override.')
param entraJwksUri string = ''

@description('Deprecated compatibility parameter: former browser scope override.')
param entraScope string = ''

@description('Azure Files share quota in GiB for the state root (corpus + policy).')
@minValue(1)
param stateShareQuotaGiB int = 100

@description('Comma-separated exact-match allow-list of external authorities (host or host:port) that the host-authority gate will serve. Leave EMPTY to default to the App Service default hostname so the built-in *.azurewebsites.net endpoint works with no extra config. Set it to name custom domains (for example "explorer.example.test,myapp.azurewebsites.net"). Requires the -allowed-host support added in PR #295.')
param allowedHosts string = ''

@description('Storage account SKU backing the state file share. Standard_LRS is single-region redundant only; see the backup/DR notes in the README.')
param storageSkuName string = 'Standard_LRS'

var uniqueSuffix = uniqueString(resourceGroup().id, namePrefix)
var storageAccountName = toLower('${replace(namePrefix, '-', '')}${uniqueSuffix}')
var storageAccountNameTrimmed = length(storageAccountName) > 24 ? substring(storageAccountName, 0, 24) : storageAccountName
var planName = '${namePrefix}-plan'
var siteName = '${namePrefix}-${uniqueSuffix}'
var identityName = '${namePrefix}-id'
var stateShareName = 'shoal-state'
var stateMountName = 'shoalstate'
var stateMountPath = '/var/lib/shoal'

resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: identityName
  location: location
}

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageAccountNameTrimmed
  location: location
  sku: {
    name: storageSkuName
  }
  kind: 'StorageV2'
  properties: {
    minimumTlsVersion: 'TLS1_2'
    allowBlobPublicAccess: false
    // The App Service SMB file mount authenticates with the account key, so
    // shared-key access must stay enabled. There is no application secret here.
    allowSharedKeyAccess: true
    supportsHttpsTrafficOnly: true
  }
}

resource fileService 'Microsoft.Storage/storageAccounts/fileServices@2023-05-01' = {
  parent: storage
  name: 'default'
}

resource stateShare 'Microsoft.Storage/storageAccounts/fileServices/shares@2023-05-01' = {
  parent: fileService
  name: stateShareName
  properties: {
    shareQuota: stateShareQuotaGiB
    enabledProtocols: 'SMB'
  }
}

resource plan 'Microsoft.Web/serverfarms@2023-12-01' = {
  name: planName
  location: location
  kind: 'linux'
  sku: {
    name: planSkuName
    // Exactly one worker. Do NOT add an autoscale settings resource: this
    // workload is a single writer against one embedded store.
    capacity: 1
  }
  properties: {
    reserved: true
  }
}

// Host-authority allow-list (PR #295's -allowed-host / SHOAL_ALLOWED_HOST).
// When the operator leaves the parameter empty, default to THIS site's own
// default hostname (for example myapp.azurewebsites.net). That is the exact
// authority a browser sends: it reaches App Service over HTTPS on the default
// port 443, so the Host header carries NO port, and PR #295 matches host
// case-insensitively with the port compared exactly (empty == empty). The
// allow-list entry is therefore the BARE hostname, never hostname:8098 (8098 is
// only the internal container port App Service forwards to, never in the public
// Host header). A non-empty parameter is passed through verbatim so an operator
// can name custom domains explicitly.
var effectiveAllowedHosts = empty(allowedHosts) ? site.properties.defaultHostName : allowedHosts
var oidcConfigured = !empty(oidcIssuer) || !empty(oidcAudience) || !empty(oidcAuthorizationClaim) || !empty(oidcDiscoveryUrl) || !empty(oidcJwksUri) || !empty(oidcBrowserClientId) || !empty(oidcBrowserScope)
// Role parameters retain their historical non-empty defaults, so they cannot
// participate in this activation test. Once another legacy parameter opts in,
// their exact values (including an explicit empty string) are passed through.
var legacyEntraConfigured = !empty(entraTenantId) || !empty(entraClientId) || !empty(entraIssuer) || !empty(entraJwksUri) || !empty(entraScope)

// App settings, as a map for the child `appsettings` config resource. They are
// set on a separate resource (not inline in siteConfig) precisely so
// SHOAL_ALLOWED_HOST can reference the site's own defaultHostName without a
// self-reference cycle.
var appSettings = union(
  {
    WEBSITES_PORT: string(containerPort)
    // The state root is a bring-your-own Azure Files mount at /var/lib/shoal,
    // not /home, so App Service /home storage is intentionally disabled.
    WEBSITES_ENABLE_APP_SERVICE_STORAGE: 'false'
    WEBSITES_CONTAINER_START_TIME_LIMIT: '600'
    SHOAL_ALLOWED_HOST: effectiveAllowedHosts
  },
  !oidcConfigured ? {} : {
    // OIDC validation and claim mapping arrive as SHOAL_OIDC_* environment
    // variables, so identifiers do not land on the command line.
    SHOAL_OIDC_ISSUER: oidcIssuer
    SHOAL_OIDC_AUDIENCE: oidcAudience
    SHOAL_OIDC_AUTHORIZATION_CLAIM: oidcAuthorizationClaim
    SHOAL_OIDC_READER_VALUES: oidcReaderValues
    SHOAL_OIDC_CONTRIBUTOR_VALUES: oidcContributorValues
  },
  empty(oidcDiscoveryUrl) ? {} : {
    SHOAL_OIDC_DISCOVERY_URL: oidcDiscoveryUrl
  },
  empty(oidcJwksUri) ? {} : {
    SHOAL_OIDC_JWKS_URI: oidcJwksUri
  },
  empty(oidcBrowserClientId) && empty(oidcBrowserScope) ? {} : {
    SHOAL_OIDC_BROWSER_CLIENT_ID: oidcBrowserClientId
    SHOAL_OIDC_BROWSER_SCOPE: oidcBrowserScope
  },
  !legacyEntraConfigured ? {} : {
    SHOAL_ENTRA_TENANT: entraTenantId
    SHOAL_ENTRA_CLIENT_ID: entraClientId
    SHOAL_ENTRA_READER_ROLES: entraReaderRoles
    SHOAL_ENTRA_CONTRIBUTOR_ROLES: entraContributorRoles
  },
  empty(entraIssuer) ? {} : {
    SHOAL_ENTRA_ISSUER: entraIssuer
  },
  empty(entraJwksUri) ? {} : {
    SHOAL_ENTRA_JWKS_URI: entraJwksUri
  },
  empty(entraScope) ? {} : {
    SHOAL_ENTRA_SCOPE: entraScope
  }
)

resource site 'Microsoft.Web/sites@2023-12-01' = {
  name: siteName
  location: location
  kind: 'app,linux,container'
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    serverFarmId: plan.id
    httpsOnly: true
    keyVaultReferenceIdentity: identity.id
    siteConfig: {
      linuxFxVersion: 'DOCKER|${containerImage}'
      alwaysOn: true
      ftpsState: 'Disabled'
      minTlsVersion: '1.2'
      http20Enabled: true
      // Single instance. Combined with plan capacity 1 and no autoscale, this
      // pins the service to exactly one running worker.
      numberOfWorkers: 1
      acrUseManagedIdentityCreds: useAcrManagedIdentity
      acrUserManagedIdentityID: useAcrManagedIdentity ? identity.properties.clientId : ''
      // Only the non-secret listen/state flags are passed here; -listen
      // 0.0.0.0 is required so App Service can reach the app. Everything else,
      // including the host-authority allow-list, is an app setting below.
      appCommandLine: '-state-dir ${stateMountPath} -listen 0.0.0.0:${string(containerPort)}'
    }
  }
}

// App settings live in their own resource so SHOAL_ALLOWED_HOST can default to
// the site's own defaultHostName without a self-reference cycle.
resource siteAppSettings 'Microsoft.Web/sites/config@2023-12-01' = {
  parent: site
  name: 'appsettings'
  properties: appSettings
}

// Mount the Azure Files share at the container state root. The access key is
// read with listKeys() at deploy time and is never stored in source control.
resource siteStorage 'Microsoft.Web/sites/config@2023-12-01' = {
  parent: site
  name: 'azurestorageaccounts'
  properties: {
    '${stateMountName}': {
      type: 'AzureFiles'
      accountName: storage.name
      shareName: stateShare.name
      mountPath: stateMountPath
      accessKey: storage.listKeys().keys[0].value
    }
  }
}

@description('Default HTTPS endpoint of the App Service instance (behind platform TLS).')
output appDefaultHostName string = 'https://${site.properties.defaultHostName}'

@description('Effective host-authority allow-list (SHOAL_ALLOWED_HOST) the gate will serve. When the allowedHosts parameter is empty this is the App Service default hostname.')
output effectiveAllowedHosts string = effectiveAllowedHosts

@description('Principal (object) ID of the user-assigned managed identity, for role assignments such as AcrPull.')
output identityPrincipalId string = identity.properties.principalId

@description('Client ID of the user-assigned managed identity.')
output identityClientId string = identity.properties.clientId

@description('Storage account backing the state root file share.')
output stateStorageAccountName string = storage.name

@description('Azure Files share holding the corpus and durable policy catalog.')
output stateShareName string = stateShare.name
