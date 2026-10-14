// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package azure is a storage.Backend over Azure Blob Storage. Each
// file.ReadAt issues a ranged DownloadStream — no state is shared between
// concurrent ReadAt calls.
//
// Azure addresses objects as account → container → blob. The account is fixed
// at construction (via AZURE_STORAGE_ACCOUNT, an explicit service URL, or a
// connection string); the container and blob come from the path:
//
//	az://container/path/to/object.rf → container="container", blob="path/to/object.rf"
//	container/path/to/object.rf      → first segment = container, rest = blob
//
// Auth (in precedence order):
//  1. WithServiceClient — a pre-built *service.Client. Reads always work;
//     writes that promote private staged blobs also require
//     WithSourceSASQuery, WithCopySourceAuthorization, or
//     WithSourceAuthorizationProvider.
//  2. WithConnectionString / AZURE_STORAGE_CONNECTION_STRING — shared-key or SAS.
//  3. AZURE_STORAGE_ACCOUNT (or WithAccount / WithServiceURL) + the default
//     Azure credential chain (env vars, managed identity, workload identity,
//     Azure CLI, etc.).
//
// For the Azurite emulator, supply the emulator's connection string via
// WithConnectionString.
package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	azblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
	"github.com/google/uuid"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

// Backend opens Azure blobs via a shared *service.Client. Safe for concurrent
// Open and concurrent ReadAt across many Files.
type Backend struct {
	svc                    *service.Client
	ops                    azureWriteOperations
	artifactOps            azureArtifactOperations
	sourceProvider         azureCopySourceProvider
	validateSourceOnCreate bool
}

var _ shstorage.Remover = (*Backend)(nil)

type azureArtifact struct {
	name         string
	versionID    *string
	lastModified time.Time
	etag         *azcore.ETag
	owned        bool
}

type azureArtifactOperations interface {
	list(context.Context, string, string) ([]azureArtifact, error)
	inspect(context.Context, string, azureArtifact) (azureArtifact, error)
	remove(context.Context, string, azureArtifact) error
}

// Option customizes Backend construction.
type Option func(*config)

type config struct {
	svc                         *service.Client
	connString                  string
	account                     string
	serviceURL                  string
	sourceAuthorizationProvider SourceAuthorizationProvider
}

// WithServiceClient supplies a pre-built service client. If set, New ignores
// all other credential options. Reads always work; writes also need staged
// source authorization, either from a SAS-bearing service URL or from
// WithSourceSASQuery, WithCopySourceAuthorization, or
// WithSourceAuthorizationProvider.
func WithServiceClient(svc *service.Client) Option {
	return func(c *config) { c.svc = svc }
}

// SourceAuthorization configures how Azure authorizes reads from a staged
// source blob during promotion. SASQuery may include or omit the leading "?";
// Authorization should be the full CopySourceAuthorization header value.
type SourceAuthorization struct {
	SASQuery      string
	Authorization string
}

// SourceAuthorizationProvider supplies per-promotion source authorization for
// staged private blobs, primarily when using WithServiceClient.
type SourceAuthorizationProvider interface {
	SourceAuthorization(context.Context, string, string) (SourceAuthorization, error)
}

// SourceAuthorizationProviderFunc adapts a function into a
// SourceAuthorizationProvider.
type SourceAuthorizationProviderFunc func(context.Context, string, string) (SourceAuthorization, error)

// SourceAuthorization implements SourceAuthorizationProvider.
func (f SourceAuthorizationProviderFunc) SourceAuthorization(ctx context.Context, containerName, blobName string) (SourceAuthorization, error) {
	return f(ctx, containerName, blobName)
}

// WithConnectionString authenticates with an Azure Storage connection string
// (shared-key or SAS). Also honored via AZURE_STORAGE_CONNECTION_STRING.
func WithConnectionString(cs string) Option {
	return func(c *config) { c.connString = cs }
}

// WithSourceSASQuery supplies an explicit SAS query string for reads from
// staged source blobs during promotion, primarily for WithServiceClient.
func WithSourceSASQuery(sasQuery string) Option {
	return WithSourceAuthorizationProvider(SourceAuthorizationProviderFunc(
		func(context.Context, string, string) (SourceAuthorization, error) {
			return SourceAuthorization{SASQuery: sasQuery}, nil
		},
	))
}

// WithCopySourceAuthorization supplies a static CopySourceAuthorization header
// value for reads from staged source blobs during promotion, primarily for
// WithServiceClient.
func WithCopySourceAuthorization(authorization string) Option {
	return WithSourceAuthorizationProvider(SourceAuthorizationProviderFunc(
		func(context.Context, string, string) (SourceAuthorization, error) {
			return SourceAuthorization{Authorization: authorization}, nil
		},
	))
}

// WithSourceAuthorizationProvider supplies explicit staged-source
// authorization for promotion, primarily when WithServiceClient hides the
// underlying credential type.
func WithSourceAuthorizationProvider(provider SourceAuthorizationProvider) Option {
	return func(c *config) { c.sourceAuthorizationProvider = provider }
}

// WithAccount sets the storage account name; the service URL is derived as
// https://<account>.blob.core.windows.net/. Also honored via
// AZURE_STORAGE_ACCOUNT.
func WithAccount(account string) Option {
	return func(c *config) { c.account = account }
}

// WithServiceURL sets an explicit blob service URL (e.g. for sovereign clouds
// or a custom endpoint). Takes precedence over WithAccount.
func WithServiceURL(url string) Option {
	return func(c *config) { c.serviceURL = url }
}

// New constructs a Backend. With no credential options it uses
// AZURE_STORAGE_CONNECTION_STRING when set, otherwise AZURE_STORAGE_ACCOUNT
// plus the default Azure credential chain.
func New(_ context.Context, opts ...Option) (*Backend, error) {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.svc != nil {
		base := rawAzureCopySourceProvider{svc: c.svc}
		return &Backend{
			svc:                    c.svc,
			ops:                    sdkAzureWriteOperations{svc: c.svc},
			artifactOps:            sdkAzureArtifactOperations{svc: c.svc},
			sourceProvider:         configuredOrAutomaticSourceProvider(base, sourceProviderFromServiceClient(c.svc), c.sourceAuthorizationProvider, true),
			validateSourceOnCreate: true,
		}, nil
	}

	connString := c.connString
	if connString == "" {
		connString = os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	}
	if connString != "" {
		svc, err := service.NewClientFromConnectionString(connString, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: NewClientFromConnectionString: %w", err)
		}
		return &Backend{
			svc:         svc,
			ops:         sdkAzureWriteOperations{svc: svc},
			artifactOps: sdkAzureArtifactOperations{svc: svc},
			sourceProvider: configuredOrAutomaticSourceProvider(
				rawAzureCopySourceProvider{svc: svc},
				sourceProviderFromConnectionString(svc, connString),
				c.sourceAuthorizationProvider,
				true,
			),
		}, nil
	}

	serviceURL := c.serviceURL
	if serviceURL == "" {
		account := c.account
		if account == "" {
			account = os.Getenv("AZURE_STORAGE_ACCOUNT")
		}
		if account == "" {
			return nil, fmt.Errorf("azure: no credentials: set AZURE_STORAGE_CONNECTION_STRING or AZURE_STORAGE_ACCOUNT, or pass WithServiceClient/WithConnectionString/WithAccount/WithServiceURL")
		}
		serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net/", account)
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure: NewDefaultAzureCredential: %w", err)
	}
	svc, err := service.NewClient(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: NewClient: %w", err)
	}
	return &Backend{
		svc:         svc,
		ops:         sdkAzureWriteOperations{svc: svc},
		artifactOps: sdkAzureArtifactOperations{svc: svc},
		sourceProvider: configuredOrAutomaticSourceProvider(
			rawAzureCopySourceProvider{svc: svc},
			tokenAzureCopySourceProvider{
				rawAzureCopySourceProvider: rawAzureCopySourceProvider{svc: svc},
				cred:                       cred,
			},
			c.sourceAuthorizationProvider,
			true,
		),
	}, nil
}

// Close is a no-op: the service client holds no persistent connection.
// Kept for symmetry with the GCS/S3 backends.
func (b *Backend) Close() error { return nil }

// Remove deletes path. A missing path is not an error.
func (b *Backend) Remove(ctx context.Context, path string) error {
	containerName, blobName, err := ParsePath(path)
	if err != nil {
		return err
	}
	_, err = b.svc.NewContainerClient(containerName).NewBlobClient(blobName).Delete(ctx, nil)
	if err != nil && !isBlobNotFound(err) {
		return fmt.Errorf("azure: remove az://%s/%s: %w", containerName, blobName, err)
	}
	return nil
}

// Open resolves path to a (container, blob) pair, calls GetProperties for size,
// and returns a File that issues a ranged DownloadStream per ReadAt.
func (b *Backend) Open(ctx context.Context, path string) (shstorage.File, error) {
	cont, name, err := ParsePath(path)
	if err != nil {
		return nil, err
	}
	bc := b.svc.NewContainerClient(cont).NewBlobClient(name)
	props, err := bc.GetProperties(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
			return nil, fmt.Errorf("%w: az://%s/%s", shstorage.ErrNotFound, cont, name)
		}
		return nil, fmt.Errorf("azure: stat az://%s/%s: %w", cont, name, err)
	}
	var size int64
	if props.ContentLength != nil {
		size = *props.ContentLength
	}
	return &file{
		blob:      bc,
		container: cont,
		name:      name,
		size:      size,
		ctx:       ctx,
	}, nil
}

// Create opens a block-blob writer. Close first stages the bytes under a
// hidden sibling blob name, then conditionally promotes that exact blob into
// the destination.
func (b *Backend) Create(ctx context.Context, path string) (shstorage.Writer, error) {
	cont, name, err := ParsePath(path)
	if err != nil {
		return nil, err
	}
	if isTemporaryStageName(name) {
		return nil, fmt.Errorf("azure: destination blob %q uses a reserved internal namespace", name)
	}
	ctx = contextOrBackground(ctx)
	stageName, err := nextTemporaryStageName(name)
	if err != nil {
		return nil, fmt.Errorf("azure: stage temporary blob for az://%s/%s: %w", cont, name, err)
	}
	if b.validateSourceOnCreate {
		if err := validatePromotionSource(ctx, b.sourceProvider, cont, stageName); err != nil {
			return nil, err
		}
	}
	ops := b.writeOperations()
	target, err := ops.head(ctx, cont, name)
	if err != nil && !isBlobNotFound(err) {
		return nil, fmt.Errorf("azure: inspect destination az://%s/%s: %w", cont, name, err)
	}
	targetExists := err == nil
	if targetExists && target.etag == nil {
		return nil, fmt.Errorf("azure: inspect destination az://%s/%s: missing ETag", cont, name)
	}
	return &writer{
		ops:            ops,
		container:      cont,
		name:           name,
		stageName:      stageName,
		writeID:        uuid.NewString(),
		ctx:            ctx,
		targetExists:   targetExists,
		target:         target,
		sourceProvider: b.sourceProvider,
		cleanupTimeout: azureCleanupTimeout,
	}, nil
}

// List returns paths of blobs directly under prefix (using delimiter "/").
// Returned paths are in az://container/blob form. Virtual "directory" prefixes
// are skipped — only leaf blobs are returned, mirroring gcs/s3 List. Internal
// staging-name shapes are reserved and omitted; pre-existing matching blobs
// remain accessible through Open and Remove.
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	cont, blobPrefix, err := ParsePath(prefix)
	if err != nil {
		return nil, err
	}
	blobPrefix = strings.TrimRight(blobPrefix, "/\\") + "/"

	var out []string
	pager := b.svc.NewContainerClient(cont).NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix: &blobPrefix,
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: list az://%s/%s: %w", cont, blobPrefix, err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil || strings.HasSuffix(*item.Name, "/") || isTemporaryStageName(*item.Name) {
				continue
			}
			out = append(out, "az://"+cont+"/"+*item.Name)
		}
	}
	return out, nil
}

// CleanupStaleArtifacts removes reserved staging blobs under prefix whose
// LastModified time is strictly before cutoff. Deletes are ETag-conditional so
// a blob replaced after enumeration is never removed.
func (b *Backend) CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (shstorage.ArtifactCleanupResult, error) {
	var result shstorage.ArtifactCleanupResult
	if err := contextOrBackground(ctx).Err(); err != nil {
		return result, err
	}
	if err := shstorage.ValidateArtifactCleanupCutoff(time.Now(), cutoff); err != nil {
		return result, fmt.Errorf("azure: %w", err)
	}
	cont, blobPrefix, err := parseArtifactCleanupPrefix(prefix)
	if err != nil {
		return result, err
	}
	if blobPrefix != "" {
		blobPrefix = strings.TrimRight(blobPrefix, "/\\") + "/"
	}
	artifacts, err := b.artifactOps.list(ctx, cont, blobPrefix)
	if err != nil {
		return result, fmt.Errorf("azure: list stale artifacts az://%s/%s: %w", cont, blobPrefix, err)
	}
	slices.SortFunc(artifacts, func(a, b azureArtifact) int {
		if order := strings.Compare(a.name, b.name); order != 0 {
			return order
		}
		var aVersion, bVersion string
		if a.versionID != nil {
			aVersion = *a.versionID
		}
		if b.versionID != nil {
			bVersion = *b.versionID
		}
		return strings.Compare(aVersion, bVersion)
	})
	var cleanupErr error
	for _, artifact := range artifacts {
		if err := contextOrBackground(ctx).Err(); err != nil {
			return result, errors.Join(cleanupErr, err)
		}
		if !isTemporaryStageName(artifact.name) {
			continue
		}
		result.Examined++
		artifact, err = b.artifactOps.inspect(ctx, cont, artifact)
		if err != nil {
			if isBlobNotFound(err) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("azure: inspect stale artifact az://%s/%s: %w", cont, artifact.name, err))
			continue
		}
		if !artifact.owned {
			continue
		}
		if artifact.lastModified.IsZero() || !artifact.lastModified.Before(cutoff) {
			continue
		}
		if artifact.etag == nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("azure: stale artifact az://%s/%s has no ETag", cont, artifact.name))
			continue
		}
		if err := b.artifactOps.remove(ctx, cont, artifact); err != nil {
			if isBlobNotFound(err) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("azure: remove stale artifact az://%s/%s: %w", cont, artifact.name, err))
			continue
		}
		result.Removed = append(result.Removed, azureArtifactPath(cont, artifact))
	}
	return result, cleanupErr
}

func azureArtifactPath(containerName string, artifact azureArtifact) string {
	path := "az://" + containerName + "/" + artifact.name
	if artifact.versionID == nil {
		return path
	}
	return path + "?versionId=" + url.QueryEscape(*artifact.versionID)
}

// ParsePath splits a path string into (container, blob). Exposed so callers
// (tests, diagnostics) can validate paths without opening.
//
// Accepted forms:
//
//	az://container/blob/path → container="container", blob="blob/path"
//	container/blob/path      → container="container", blob="blob/path"
func ParsePath(path string) (containerName, blobName string, err error) {
	trimmed := strings.TrimPrefix(path, "az://")
	idx := strings.IndexByte(trimmed, '/')
	if idx < 0 || idx == len(trimmed)-1 {
		return "", "", fmt.Errorf("azure: invalid path %q (want az://container/blob or container/blob)", path)
	}
	containerName = trimmed[:idx]
	blobName = trimmed[idx+1:]
	if containerName == "" {
		return "", "", fmt.Errorf("azure: empty container in %q", path)
	}
	return containerName, blobName, nil
}

func parseArtifactCleanupPrefix(path string) (container, prefix string, err error) {
	trimmed := strings.TrimPrefix(path, "az://")
	container, prefix, _ = strings.Cut(trimmed, "/")
	if container == "" {
		return "", "", fmt.Errorf("azure: empty container in %q", path)
	}
	return container, strings.TrimLeft(prefix, "/"), nil
}

const (
	maxBlobNameChars      = 1024
	tempStageNamePrefix   = ".shoal-tmp-"
	tempStageHashHexLen   = 4
	tempStageRandomHexLen = 10
	tempStageComponentLen = len(tempStageNamePrefix) + tempStageHashHexLen + tempStageRandomHexLen
	legacyStageDirPrefix  = ".shoal-tmp/"
	azureSourceSASExpiry  = 5 * time.Minute
)

var randomStageNameToken = func() (string, error) {
	buf := make([]byte, tempStageRandomHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random staging token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func nextTemporaryStageName(name string) (string, error) {
	prefix, err := temporaryStageNamePrefixFor(name)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(name))
	token, err := randomStageNameToken()
	if err != nil {
		return "", err
	}
	hashHex := hex.EncodeToString(hash[:tempStageHashHexLen/2])
	if len(hashHex) < tempStageHashHexLen || len(token) < tempStageRandomHexLen {
		return "", fmt.Errorf("temporary blob token material too short")
	}
	component := tempStageNamePrefix + token[:tempStageRandomHexLen] + hashHex[:tempStageHashHexLen]
	available := maxBlobNameChars - utf8.RuneCountInString(prefix)
	if available < tempStageComponentLen {
		return "", fmt.Errorf(
			"blob prefix %q leaves %d characters for a temporary blob; need at least %d",
			prefix, available, tempStageComponentLen,
		)
	}
	return prefix + component, nil
}

func temporaryStageNamePrefixFor(name string) (string, error) {
	prefix := stageNameParentPrefix(name)
	if maxBlobNameChars-utf8.RuneCountInString(prefix) < tempStageComponentLen {
		available := maxBlobNameChars - utf8.RuneCountInString(prefix)
		return "", fmt.Errorf(
			"blob prefix %q leaves %d characters for a temporary blob; need at least %d",
			prefix, available, tempStageComponentLen,
		)
	}
	return prefix, nil
}

func stageNameParentPrefix(name string) string {
	if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
		return name[:idx+1]
	}
	return ""
}

func isTemporaryStageName(name string) bool {
	base := name
	if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
		base = base[idx+1:]
	}
	return isLegacyTemporaryStageName(name) || isGeneratedTemporaryStageComponent(base)
}

func isLegacyTemporaryStageName(name string) bool {
	if !strings.HasPrefix(name, legacyStageDirPrefix) {
		return false
	}
	remainder := name[len(legacyStageDirPrefix):]
	if remainder == "" || strings.ContainsRune(remainder, '/') {
		return false
	}
	if remainder != strings.ToLower(remainder) {
		return false
	}
	_, err := uuid.Parse(remainder)
	return err == nil
}

func isGeneratedTemporaryStageComponent(name string) bool {
	if len(name) != tempStageComponentLen || !strings.HasPrefix(name, tempStageNamePrefix) {
		return false
	}
	token := name[len(tempStageNamePrefix) : len(tempStageNamePrefix)+tempStageRandomHexLen]
	hash := name[len(tempStageNamePrefix)+tempStageRandomHexLen:]
	return isLowerHex(token) && isLowerHex(hash)
}

type azureCopySource struct {
	url           string
	authorization *string
}

type azureCopySourceProvider interface {
	source(context.Context, string, string) (azureCopySource, error)
}

type configuredAzureCopySourceProvider struct {
	rawAzureCopySourceProvider
	provider SourceAuthorizationProvider
}

func (p configuredAzureCopySourceProvider) source(ctx context.Context, containerName, blobName string) (azureCopySource, error) {
	if p.provider == nil {
		return azureCopySource{}, fmt.Errorf("azure: no source authorization configured")
	}
	auth, err := p.provider.SourceAuthorization(ctx, containerName, blobName)
	if err != nil {
		return azureCopySource{}, err
	}
	if auth.Authorization == "" && auth.SASQuery == "" {
		return azureCopySource{}, fmt.Errorf("azure: no source authorization configured")
	}
	sourceURL, err := url.Parse(p.blobURL(containerName, blobName))
	if err != nil {
		return azureCopySource{}, fmt.Errorf("azure: parse source blob URL: %w", err)
	}
	if auth.SASQuery != "" {
		sourceURL.RawQuery = strings.TrimPrefix(auth.SASQuery, "?")
	}
	source := azureCopySource{url: sourceURL.String()}
	if auth.Authorization != "" {
		source.authorization = &auth.Authorization
	}
	return source, nil
}

type rawAzureCopySourceProvider struct {
	svc *service.Client
}

func (p rawAzureCopySourceProvider) source(_ context.Context, containerName, blobName string) (azureCopySource, error) {
	return azureCopySource{url: p.blobURL(containerName, blobName)}, nil
}

func (p rawAzureCopySourceProvider) blobURL(containerName, blobName string) string {
	return p.svc.NewContainerClient(containerName).NewBlobClient(blobName).URL()
}

type sasAzureCopySourceProvider struct {
	rawAzureCopySourceProvider
	sasQuery string
}

func (p sasAzureCopySourceProvider) source(_ context.Context, containerName, blobName string) (azureCopySource, error) {
	sourceURL, err := url.Parse(p.blobURL(containerName, blobName))
	if err != nil {
		return azureCopySource{}, fmt.Errorf("azure: parse source blob URL: %w", err)
	}
	sourceURL.RawQuery = strings.TrimPrefix(p.sasQuery, "?")
	return azureCopySource{url: sourceURL.String()}, nil
}

type sharedKeyAzureCopySourceProvider struct {
	rawAzureCopySourceProvider
	cred *azblob.SharedKeyCredential
}

func (p sharedKeyAzureCopySourceProvider) source(_ context.Context, containerName, blobName string) (azureCopySource, error) {
	blobURL := p.blobURL(containerName, blobName)
	client, err := blob.NewClientWithSharedKeyCredential(blobURL, p.cred, nil)
	if err != nil {
		return azureCopySource{}, fmt.Errorf("azure: build shared-key source blob client: %w", err)
	}
	sourceURL, err := client.GetSASURL(sas.BlobPermissions{Read: true}, time.Now().Add(azureSourceSASExpiry), nil)
	if err != nil {
		return azureCopySource{}, fmt.Errorf("azure: sign source blob URL: %w", err)
	}
	return azureCopySource{url: sourceURL}, nil
}

type tokenAzureCopySourceProvider struct {
	rawAzureCopySourceProvider
	cred azcore.TokenCredential
}

func (p tokenAzureCopySourceProvider) source(ctx context.Context, containerName, blobName string) (azureCopySource, error) {
	token, err := p.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://storage.azure.com/.default"}})
	if err != nil {
		return azureCopySource{}, fmt.Errorf("azure: acquire source copy token: %w", err)
	}
	authorization := "Bearer " + token.Token
	return azureCopySource{
		url:           p.blobURL(containerName, blobName),
		authorization: &authorization,
	}, nil
}

func sourceProviderFromServiceClient(svc *service.Client) azureCopySourceProvider {
	base := rawAzureCopySourceProvider{svc: svc}
	serviceURL, err := url.Parse(svc.URL())
	if err != nil || serviceURL.RawQuery == "" {
		return nil
	}
	return sasAzureCopySourceProvider{rawAzureCopySourceProvider: base, sasQuery: serviceURL.RawQuery}
}

func configuredOrAutomaticSourceProvider(
	base rawAzureCopySourceProvider,
	automatic azureCopySourceProvider,
	explicit SourceAuthorizationProvider,
	allowAutomatic bool,
) azureCopySourceProvider {
	if explicit != nil {
		return configuredAzureCopySourceProvider{
			rawAzureCopySourceProvider: base,
			provider:                   explicit,
		}
	}
	if allowAutomatic {
		return automatic
	}
	return nil
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	if value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value)
}

func sourceProviderFromConnectionString(svc *service.Client, connString string) azureCopySourceProvider {
	values := parseConnectionString(connString)
	base := rawAzureCopySourceProvider{svc: svc}
	if sasQuery := values["SharedAccessSignature"]; sasQuery != "" {
		return sasAzureCopySourceProvider{rawAzureCopySourceProvider: base, sasQuery: sasQuery}
	}
	accountName := values["AccountName"]
	accountKey := values["AccountKey"]
	if accountName == "" || accountKey == "" {
		return base
	}
	cred, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return base
	}
	return sharedKeyAzureCopySourceProvider{rawAzureCopySourceProvider: base, cred: cred}
}

func parseConnectionString(connString string) map[string]string {
	values := make(map[string]string)
	for _, part := range strings.Split(connString, ";") {
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	return values
}

// file is the Azure File implementation. Each ReadAt issues a fresh ranged
// DownloadStream — stateless per call so concurrent ReadAts don't share reader
// state.
type file struct {
	blob      *blob.Client
	container string
	name      string
	size      int64
	ctx       context.Context //nolint:containedctx
}

func (f *file) Size() int64 { return f.size }

// Close is a no-op — no per-file resources are held.
func (f *file) Close() error { return nil }

// ReadAt issues a single ranged DownloadStream covering [off, off+len(p)). On
// short reads at EOF returns (n, io.EOF) per io.ReaderAt's contract.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("azure: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= f.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if off+want > f.size {
		want = f.size - off
	}
	resp, err := f.blob.DownloadStream(f.ctx, &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: off, Count: want},
	})
	if err != nil {
		return 0, fmt.Errorf("azure: download az://%s/%s off=%d: %w", f.container, f.name, off, err)
	}
	defer resp.Body.Close()

	n, err := io.ReadFull(resp.Body, p[:want])
	if err != nil {
		// io.ReadFull returns io.ErrUnexpectedEOF for short reads; map that to
		// a regular EOF if we're at the blob's end so callers don't have to
		// handle two flavors.
		if err == io.ErrUnexpectedEOF && off+int64(n) >= f.size {
			err = io.EOF
		} else {
			return n, fmt.Errorf("azure: read body az://%s/%s off=%d: %w", f.container, f.name, off, err)
		}
	}
	if int64(n) < int64(len(p)) {
		// Caller asked for more bytes than the blob contains at this offset.
		return n, io.EOF
	}
	return n, nil
}

type azureObjectState struct {
	etag      *azcore.ETag
	versionID *string
	size      int64
	metadata  map[string]*string
}

type azureWriteOperations interface {
	head(context.Context, string, string) (azureObjectState, error)
	uploadStage(context.Context, string, string, []byte, string) (azureObjectState, error)
	promote(context.Context, string, string, string, azureCopySource, azureObjectState, string, bool, azureObjectState) error
	deleteStage(context.Context, string, string, azureObjectState) error
}

type sdkAzureWriteOperations struct {
	svc *service.Client
}

type sdkAzureArtifactOperations struct {
	svc *service.Client
}

func (o sdkAzureArtifactOperations) list(ctx context.Context, containerName, prefix string) ([]azureArtifact, error) {
	pager := o.svc.NewContainerClient(containerName).NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:  &prefix,
		Include: container.ListBlobsInclude{Metadata: true, Versions: true},
	})
	var artifacts []azureArtifact
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil || item.Properties == nil {
				continue
			}
			artifact := azureArtifact{
				name:      *item.Name,
				versionID: item.VersionID,
				etag:      item.Properties.ETag,
				owned:     metadataValue(item.Metadata, "shoal-write-id") != "",
			}
			if item.Properties.LastModified != nil {
				artifact.lastModified = *item.Properties.LastModified
			}
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, nil
}

func (o sdkAzureArtifactOperations) remove(ctx context.Context, containerName string, artifact azureArtifact) error {
	client, err := azureVersionClient(o.svc.NewContainerClient(containerName).NewBlobClient(artifact.name), artifact.versionID)
	if err != nil {
		return err
	}
	_, err = client.Delete(ctx, &blob.DeleteOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: artifact.etag},
		},
	})
	return err
}

func (o sdkAzureArtifactOperations) inspect(ctx context.Context, containerName string, artifact azureArtifact) (azureArtifact, error) {
	client, err := azureVersionClient(o.svc.NewContainerClient(containerName).NewBlobClient(artifact.name), artifact.versionID)
	if err != nil {
		return artifact, err
	}
	out, err := client.GetProperties(ctx, nil)
	if err != nil {
		return artifact, err
	}
	artifact.etag = out.ETag
	if out.LastModified != nil {
		artifact.lastModified = *out.LastModified
	}
	artifact.owned = metadataValue(out.Metadata, "shoal-write-id") != ""
	return artifact, nil
}

func azureVersionClient(client *blob.Client, versionID *string) (*blob.Client, error) {
	if versionID == nil {
		return client, nil
	}
	return client.WithVersionID(*versionID)
}

func (o sdkAzureWriteOperations) head(
	ctx context.Context, containerName, name string,
) (azureObjectState, error) {
	out, err := o.svc.NewContainerClient(containerName).NewBlobClient(name).GetProperties(ctx, nil)
	if err != nil {
		return azureObjectState{}, err
	}
	state := azureObjectState{etag: out.ETag, versionID: out.VersionID, metadata: out.Metadata}
	if out.ContentLength != nil {
		state.size = *out.ContentLength
	}
	return state, nil
}

func (o sdkAzureWriteOperations) uploadStage(
	ctx context.Context,
	containerName, name string,
	data []byte,
	writeID string,
) (azureObjectState, error) {
	out, err := o.svc.NewContainerClient(containerName).NewBlockBlobClient(name).UploadBuffer(
		ctx,
		data,
		&blockblob.UploadBufferOptions{
			Metadata: map[string]*string{"shoal-write-id": to.Ptr(writeID)},
			AccessConditions: &blob.AccessConditions{
				ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
			},
		},
	)
	if err != nil {
		return azureObjectState{}, err
	}
	return azureObjectState{
		etag:      out.ETag,
		versionID: out.VersionID,
		size:      int64(len(data)),
		metadata:  map[string]*string{"shoal-write-id": to.Ptr(writeID)},
	}, nil
}

func (o sdkAzureWriteOperations) promote(
	ctx context.Context,
	containerName, stageName, name string,
	source azureCopySource,
	stage azureObjectState,
	writeID string,
	targetExists bool,
	target azureObjectState,
) error {
	bb := o.svc.NewContainerClient(containerName).NewBlockBlobClient(name)
	return promoteStagedBlob(
		ctx,
		sdkAzureBlobPromoter{client: bb},
		source,
		stage,
		targetExists,
		target,
	)
}

type azureBlobPromoter interface {
	UploadBlobFromURL(context.Context, string, *blockblob.UploadBlobFromURLOptions) error
}

type sdkAzureBlobPromoter struct {
	client *blockblob.Client
}

func (p sdkAzureBlobPromoter) UploadBlobFromURL(
	ctx context.Context,
	source string,
	options *blockblob.UploadBlobFromURLOptions,
) error {
	_, err := p.client.UploadBlobFromURL(ctx, source, options)
	return err
}

func promoteStagedBlob(
	ctx context.Context,
	promoter azureBlobPromoter,
	source azureCopySource,
	stage azureObjectState,
	targetExists bool,
	target azureObjectState,
) error {
	conditions := &blob.ModifiedAccessConditions{}
	if targetExists {
		conditions.IfMatch = target.etag
	} else {
		conditions.IfNoneMatch = to.Ptr(azcore.ETagAny)
	}
	return promoter.UploadBlobFromURL(ctx, source.url, &blockblob.UploadBlobFromURLOptions{
		CopySourceAuthorization:  source.authorization,
		CopySourceBlobProperties: to.Ptr(false),
		Metadata:                 stage.metadata,
		SourceModifiedAccessConditions: &blob.SourceModifiedAccessConditions{
			SourceIfMatch: stage.etag,
		},
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: conditions,
		},
	})
}

func (o sdkAzureWriteOperations) deleteStage(
	ctx context.Context, containerName, name string, stage azureObjectState,
) error {
	client, err := azureVersionClient(o.svc.NewContainerClient(containerName).NewBlobClient(name), stage.versionID)
	if err != nil {
		return err
	}
	_, err = client.Delete(
		ctx,
		&blob.DeleteOptions{
			AccessConditions: &blob.AccessConditions{
				ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: stage.etag},
			},
		},
	)
	return err
}

var azureCleanupTimeout = 10 * time.Second

const committedCleanupAttempts = 2
const maxUploadBlobFromURLBytes int64 = 5000 << 20

type destinationVerificationState uint8

const (
	destinationVerificationCommitted destinationVerificationState = iota
	destinationVerificationNotCommitted
	destinationVerificationIndeterminate
)

func (b *Backend) writeOperations() azureWriteOperations {
	if b.ops != nil {
		return b.ops
	}
	return sdkAzureWriteOperations{svc: b.svc}
}

// writer buffers bytes in memory, stages them, and conditionally publishes
// them on Close.
type writer struct {
	ops                    azureWriteOperations
	container              string
	name                   string
	stageName              string
	writeID                string
	ctx                    context.Context //nolint:containedctx
	targetExists           bool
	target                 azureObjectState
	sourceProvider         azureCopySourceProvider
	stage                  azureObjectState
	stageCreated           bool
	stageUnknown           bool
	stageOwned             bool
	promotionIndeterminate bool
	cleanupTimeout         time.Duration
	buf                    bytes.Buffer
	closed                 bool
	abortRequested         bool
	aborted                bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.aborted || w.abortRequested {
		return 0, fmt.Errorf("azure: writer already aborted")
	}
	if w.closed || w.promotionIndeterminate {
		return 0, fmt.Errorf("azure: writer already closed")
	}
	return w.buf.Write(p)
}

func (w *writer) Close() error {
	if w.aborted || w.abortRequested {
		return fmt.Errorf("azure: writer already aborted")
	}
	if w.closed {
		w.cleanupCommittedStageBestEffort()
		return nil
	}
	if resolved, err := w.resolveIndeterminateClose(); resolved || err != nil {
		return err
	}
	if err := validateAzurePromotionSize(int64(w.buf.Len())); err != nil {
		return err
	}
	if !w.stageCreated {
		w.stageUnknown = true
		stage, err := w.ops.uploadStage(w.ctx, w.container, w.stageName, w.buf.Bytes(), w.writeID)
		if err != nil {
			if verified, verifyErr := w.verifyStage(); verifyErr != nil || !verified {
				return errors.Join(
					fmt.Errorf("azure: stage upload az://%s/%s: %w", w.container, w.stageName, err),
					verifyErr,
					w.cleanupStage(),
				)
			}

			stage = w.stage
		}
		w.rememberStage(stage)
		w.stageUnknown = false
	}
	if w.stage.etag == nil {
		if verified, err := w.verifyStage(); err != nil || !verified || w.stage.etag == nil {
			return errors.Join(
				fmt.Errorf("azure: temporary blob az://%s/%s is missing an ETag", w.container, w.stageName),
				err,
				w.cleanupStage(),
			)
		}
	}
	source, err := w.promotionSource()
	if err != nil {
		return errors.Join(err, w.cleanupStage())
	}
	if err := w.ops.promote(
		w.ctx, w.container, w.stageName, w.name, source, w.stage, w.writeID, w.targetExists, w.target,
	); err != nil {
		verifyState, verifyErr := w.verifyDestination()
		if verifyState == destinationVerificationCommitted {
			w.closed = true
			w.cleanupCommittedStageBestEffort()
			return nil
		}
		if isAmbiguousPromotionError(err) {
			w.promotionIndeterminate = true
			return errors.Join(
				w.indeterminatePromotionError(),
				fmt.Errorf("azure: promote az://%s/%s to az://%s/%s: %w",
					w.container, w.stageName, w.container, w.name, err),
				verifyErr,
			)
		}
		if verifyErr != nil || verifyState == destinationVerificationCommitted {
			return errors.Join(
				fmt.Errorf("azure: promote az://%s/%s to az://%s/%s: %w",
					w.container, w.stageName, w.container, w.name, err),
				verifyErr,
				w.cleanupStage(),
			)
		}
		return errors.Join(
			fmt.Errorf("azure: promote az://%s/%s to az://%s/%s: %w",
				w.container, w.stageName, w.container, w.name, err),
			w.cleanupStage(),
		)
	}
	w.closed = true
	w.cleanupCommittedStageBestEffort()
	return nil
}

func validateAzurePromotionSize(size int64) error {
	if size > maxUploadBlobFromURLBytes {
		return fmt.Errorf(
			"azure: staged write size %d exceeds Put Blob From URL promotion limit %d",
			size,
			maxUploadBlobFromURLBytes,
		)
	}
	return nil
}

func (w *writer) Abort() error {
	if w.aborted {
		return nil
	}
	if w.closed {
		return fmt.Errorf("azure: writer already closed")
	}
	if w.promotionIndeterminate {
		verifyState, verifyErr := w.verifyDestination()
		switch {
		case verifyErr != nil:
			return errors.Join(w.indeterminateAbortError(), verifyErr)
		case verifyState == destinationVerificationCommitted:
			return w.indeterminateAbortError()
		case verifyState == destinationVerificationIndeterminate:
			return w.indeterminateAbortError()
		default:
			w.promotionIndeterminate = false
		}
	}
	w.abortRequested = true
	if err := w.cleanupStage(); err != nil {
		return err
	}
	w.aborted = true
	w.buf.Reset()
	return nil
}

func (w *writer) verifyStage() (bool, error) {
	_, found, err := w.lookupStage()
	if err != nil {
		return false, err
	}
	if !found || !w.stageCreated {
		return false, nil
	}
	return true, nil
}

func (w *writer) resolveIndeterminateClose() (bool, error) {
	if !w.promotionIndeterminate {
		return false, nil
	}
	verifyState, verifyErr := w.verifyDestination()
	if verifyErr != nil {
		return true, errors.Join(w.indeterminatePromotionError(), verifyErr)
	}
	switch verifyState {
	case destinationVerificationCommitted:
		w.promotionIndeterminate = false
		w.closed = true
		w.cleanupCommittedStageBestEffort()
		return true, nil
	case destinationVerificationNotCommitted:
		w.promotionIndeterminate = false
		return false, nil
	default:
		return true, w.indeterminatePromotionError()
	}
}

func (w *writer) verifyDestination() (destinationVerificationState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), w.cleanupTimeout)
	defer cancel()
	state, err := w.ops.head(ctx, w.container, w.name)
	if err != nil {
		if isBlobNotFound(err) {
			if w.targetExists {
				return destinationVerificationIndeterminate, nil
			}
			return destinationVerificationNotCommitted, nil
		}
		return destinationVerificationIndeterminate, fmt.Errorf("azure: verify destination az://%s/%s: %w", w.container, w.name, err)
	}
	if state.size == int64(w.buf.Len()) &&
		metadataValue(state.metadata, "shoal-write-id") == w.writeID {
		return destinationVerificationCommitted, nil
	}
	if w.targetExists && equalETags(state.etag, w.target.etag) {
		return destinationVerificationNotCommitted, nil
	}
	return destinationVerificationIndeterminate, nil
}

func (w *writer) cleanupCommittedStageBestEffort() {
	if err := w.retryCommittedCleanup(w.cleanupStage); err != nil {
		slog.Warn(
			"azure committed write left temporary blob pending cleanup",
			"target", "az://"+w.container+"/"+w.name,
			"stage", "az://"+w.container+"/"+w.stageName,
			"error", err,
		)
	}
}

func (w *writer) cleanupStage() error {
	if w.ops == nil || w.container == "" || w.stageName == "" {
		w.clearStage()
		return nil
	}
	if !w.stageOwned {
		if !w.stageUnknown {
			return nil
		}
		owned, err := w.lookupOwnedStageForCleanup()
		if err != nil {
			return err
		}
		if !owned {
			w.clearStage()
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.cleanupTimeout)
	defer cancel()
	if err := w.ops.deleteStage(ctx, w.container, w.stageName, w.stage); err != nil && !isBlobNotFound(err) {
		return fmt.Errorf("azure: remove temporary blob az://%s/%s: %w", w.container, w.stageName, err)
	}
	w.clearStage()
	return nil
}

func (w *writer) lookupStage() (azureObjectState, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), w.cleanupTimeout)
	defer cancel()
	state, err := w.ops.head(ctx, w.container, w.stageName)
	if err != nil {
		if isBlobNotFound(err) {
			w.clearStage()
			return azureObjectState{}, false, nil
		}
		return azureObjectState{}, false, fmt.Errorf("azure: verify temporary blob az://%s/%s: %w", w.container, w.stageName, err)
	}
	w.rememberStage(state)
	w.stageUnknown = false
	return state, true, nil
}

func (w *writer) lookupOwnedStageForCleanup() (bool, error) {
	_, found, err := w.lookupStage()
	if err != nil {
		return false, err
	}
	return found && w.stageOwned, nil
}

func (w *writer) rememberStage(state azureObjectState) {
	w.stage = state
	w.stageOwned = metadataValue(state.metadata, "shoal-write-id") == w.writeID
	w.stageCreated = w.stageOwned && state.size == int64(w.buf.Len())
}

func (w *writer) clearStage() {
	w.stage = azureObjectState{}
	w.stageCreated = false
	w.stageUnknown = false
	w.stageOwned = false
	w.promotionIndeterminate = false
}

func (w *writer) retryCommittedCleanup(cleanup func() error) error {
	var combined error
	for attempt := 0; attempt < committedCleanupAttempts; attempt++ {
		if err := cleanup(); err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		return nil
	}
	return combined
}

func (w *writer) promotionSource() (azureCopySource, error) {
	if w.sourceProvider == nil {
		return azureCopySource{}, fmt.Errorf("azure: no source authorization configured for az://%s/%s", w.container, w.stageName)
	}
	source, err := w.sourceProvider.source(w.ctx, w.container, w.stageName)
	if err != nil {
		return azureCopySource{}, fmt.Errorf("azure: authorize staged source az://%s/%s: %w", w.container, w.stageName, err)
	}
	return source, nil
}

// metadataValue looks up blob metadata case-insensitively because the service
// returns metadata through response headers, which arrive canonicalized (for
// example "Shoal-Write-Id" rather than "shoal-write-id").
func metadataValue(metadata map[string]*string, key string) string {
	if value, ok := metadata[key]; ok {
		if value == nil {
			return ""
		}
		return *value
	}
	for name, value := range metadata {
		if strings.EqualFold(name, key) {
			if value == nil {
				return ""
			}
			return *value
		}
	}
	return ""
}

func equalETags(a, b *azcore.ETag) bool {
	return a != nil && b != nil && a.Equals(*b)
}

func isBlobNotFound(err error) bool {
	return bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound)
}

func isAmbiguousPromotionError(err error) bool {
	if err == nil || isBlobNotFound(err) {
		return false
	}
	if bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists) {
		return false
	}
	text := strings.ToLower(err.Error())
	return !strings.Contains(text, "precondition")
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (w *writer) indeterminatePromotionError() error {
	return fmt.Errorf(
		"azure: promotion of az://%s/%s is indeterminate; staged blob az://%s/%s retained for retry",
		w.container, w.name, w.container, w.stageName,
	)
}

func (w *writer) indeterminateAbortError() error {
	return fmt.Errorf(
		"azure: cannot abort indeterminate promotion of az://%s/%s; staged blob az://%s/%s retained for retry",
		w.container, w.name, w.container, w.stageName,
	)
}

func validatePromotionSource(
	ctx context.Context,
	provider azureCopySourceProvider,
	containerName, blobName string,
) error {
	if provider == nil {
		return fmt.Errorf("azure: no source authorization configured for az://%s/%s", containerName, blobName)
	}
	source, err := provider.source(ctx, containerName, blobName)
	if err != nil {
		return fmt.Errorf("azure: authorize staged source az://%s/%s: %w", containerName, blobName, err)
	}
	if source.authorization != nil {
		return nil
	}
	sourceURL, err := url.Parse(source.url)
	if err != nil {
		return fmt.Errorf("azure: authorize staged source az://%s/%s: %w", containerName, blobName, err)
	}
	if sourceURL.RawQuery == "" {
		return fmt.Errorf("azure: no source authorization configured for az://%s/%s", containerName, blobName)
	}
	return nil
}
