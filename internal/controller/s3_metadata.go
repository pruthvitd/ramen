// SPDX-FileCopyrightText: The RamenDR authors
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// S3StoreMetadataKey is the S3 object key for the metadata file
	S3StoreMetadataKey = ".metadata"

	// S3StoreSchemaVersion is the current schema version
	S3StoreSchemaVersion = "v1"
)

// ResourceTypeInfo contains information about a resource type backed up to S3
type ResourceTypeInfo struct {
	// Count is the number of resources of this type
	Count int `json:"count"`

	// APIVersion is the Kubernetes API version of this resource type
	APIVersion string `json:"apiVersion,omitempty"`

	// CaptureNumber is the capture/epoch number for kubeObjects
	CaptureNumber int64 `json:"captureNumber,omitempty"`
}

// S3StoreMetadata contains metadata about the S3 store contents
// This file is stored at <namespace>/<vrgname>/.metadata in S3
type S3StoreMetadata struct {
	// SchemaVersion indicates the S3 store structure version
	// Used for future migrations (e.g., v1 -> v2 when adding encryption)
	SchemaVersion string `json:"schemaVersion"`

	// VRGName is the name of the VolumeReplicationGroup
	VRGName string `json:"vrgName"`

	// VRGNamespace is the namespace of the VolumeReplicationGroup
	VRGNamespace string `json:"vrgNamespace"`

	// CurrentEpoch is the current capture number/epoch for kubeObjectProtection
	CurrentEpoch int64 `json:"currentEpoch"`

	// LastUpdatedBy indicates which cluster last updated this S3 store
	LastUpdatedBy string `json:"lastUpdatedBy"`

	// LastUpdatedAt is the timestamp of the last update
	LastUpdatedAt metav1.Time `json:"lastUpdatedAt"`

	// ProtectionStatus indicates the overall protection status
	// Values: "InProgress", "Complete", "Failed"
	ProtectionStatus string `json:"protectionStatus"`

	// S3ProfileName is the name of the S3 profile used for this store
	// Used to detect S3 profile changes in DRPolicy
	S3ProfileName string `json:"s3ProfileName"`

	// ResourceTypes maps resource type names to their metadata
	// Example keys: "persistentVolumes", "persistentVolumeClaims", "volumeReplicationGroup", "kubeObjects"
	ResourceTypes map[string]ResourceTypeInfo `json:"resourceTypes"`
}

// GetS3StoreMetadata retrieves the metadata file from S3
// Returns (metadata, found, error)
// - found=false if metadata file doesn't exist (first-time upload)
// - error if S3 operation fails
func GetS3StoreMetadata(objectStore ObjectStorer, pathPrefix string) (*S3StoreMetadata, bool, error) {
	metadataKey := pathPrefix + S3StoreMetadataKey

	var metadata S3StoreMetadata

	err := objectStore.DownloadObject(metadataKey, &metadata)
	if err != nil {
		// Check if file doesn't exist (first-time case)
		if errors.Is(err, fs.ErrNotExist) || isAwsErrCodeNoSuchKey(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("failed to download S3 metadata from %s: %w", metadataKey, err)
	}

	return &metadata, true, nil
}

// CreateS3StoreMetadata creates a new metadata file in S3
// Returns error if metadata already exists or S3 operation fails
func CreateS3StoreMetadata(objectStore ObjectStorer, pathPrefix string, metadata S3StoreMetadata) error {
	// Check if metadata already exists
	_, found, err := GetS3StoreMetadata(objectStore, pathPrefix)
	if err != nil {
		return fmt.Errorf("failed to check existing metadata: %w", err)
	}

	if found {
		return fmt.Errorf("metadata already exists at %s%s", pathPrefix, S3StoreMetadataKey)
	}

	return updateS3StoreMetadataInternal(objectStore, pathPrefix, metadata)
}

// UpdateS3StoreMetadata updates an existing metadata file in S3
// Creates the file if it doesn't exist (idempotent)
func UpdateS3StoreMetadata(objectStore ObjectStorer, pathPrefix string, metadata S3StoreMetadata) error {
	return updateS3StoreMetadataInternal(objectStore, pathPrefix, metadata)
}

// updateS3StoreMetadataInternal is the internal implementation for uploading metadata
func updateS3StoreMetadataInternal(objectStore ObjectStorer, pathPrefix string, metadata S3StoreMetadata) error {
	metadataKey := pathPrefix + S3StoreMetadataKey

	// Upload to S3 - UploadObject handles serialization
	if err := objectStore.UploadObject(metadataKey, metadata); err != nil {
		return fmt.Errorf("failed to upload S3 metadata to %s: %w", metadataKey, err)
	}

	return nil
}

// InitializeS3StoreMetadata creates a new metadata structure with default values
// This is used when creating metadata for the first time
func InitializeS3StoreMetadata(vrgName, vrgNamespace, s3ProfileName, clusterName string) S3StoreMetadata {
	return S3StoreMetadata{
		SchemaVersion:    S3StoreSchemaVersion,
		VRGName:          vrgName,
		VRGNamespace:     vrgNamespace,
		CurrentEpoch:     0,
		LastUpdatedBy:    clusterName,
		LastUpdatedAt:    metav1.Now(),
		ProtectionStatus: "InProgress",
		S3ProfileName:    s3ProfileName,
		ResourceTypes:    make(map[string]ResourceTypeInfo),
	}
}

// ShouldReuploadToS3Store determines if resources should be re-uploaded to S3
// based on metadata comparison
// Returns (shouldReupload, reason)
func ShouldReuploadToS3Store(metadata *S3StoreMetadata, currentS3ProfileName string) (bool, string) {
	if metadata == nil {
		return true, "metadata not found (first-time upload)"
	}

	// Check for S3 profile change
	if metadata.S3ProfileName != currentS3ProfileName {
		return true, fmt.Sprintf("S3 profile changed from %s to %s",
			metadata.S3ProfileName, currentS3ProfileName)
	}

	// Check for schema version change (future use)
	if metadata.SchemaVersion != S3StoreSchemaVersion {
		return true, fmt.Sprintf("schema version mismatch: store=%s, current=%s",
			metadata.SchemaVersion, S3StoreSchemaVersion)
	}

	return false, ""
}

// UpdateResourceTypeInfo updates the resource type information in metadata
func (m *S3StoreMetadata) UpdateResourceTypeInfo(resourceType string, count int, apiVersion string) {
	if m.ResourceTypes == nil {
		m.ResourceTypes = make(map[string]ResourceTypeInfo)
	}

	m.ResourceTypes[resourceType] = ResourceTypeInfo{
		Count:      count,
		APIVersion: apiVersion,
	}
}

// UpdateKubeObjectsInfo updates the kubeObjects resource type with capture number
func (m *S3StoreMetadata) UpdateKubeObjectsInfo(count int, captureNumber int64) {
	if m.ResourceTypes == nil {
		m.ResourceTypes = make(map[string]ResourceTypeInfo)
	}

	m.ResourceTypes["kubeObjects"] = ResourceTypeInfo{
		Count:         count,
		CaptureNumber: captureNumber,
	}
}

// SetProtectionComplete marks the protection as complete
func (m *S3StoreMetadata) SetProtectionComplete(clusterName string) {
	m.ProtectionStatus = "Complete"
	m.LastUpdatedBy = clusterName
	m.LastUpdatedAt = metav1.Now()
}

// SetProtectionFailed marks the protection as failed
func (m *S3StoreMetadata) SetProtectionFailed(clusterName string) {
	m.ProtectionStatus = "Failed"
	m.LastUpdatedBy = clusterName
	m.LastUpdatedAt = metav1.Now()
}

// ShouldReupload determines if re-upload is needed
func (m *S3StoreMetadata) ShouldReupload(currentS3ProfileName string) (bool, string) {
	if m.S3ProfileName != currentS3ProfileName {
		return true, fmt.Sprintf("S3 profile changed from %s to %s",
			m.S3ProfileName, currentS3ProfileName)
	}

	if m.SchemaVersion != S3StoreSchemaVersion {
		return true, fmt.Sprintf("schema version mismatch: store=%s, current=%s",
			m.SchemaVersion, S3StoreSchemaVersion)
	}

	return false, ""
}

// ============================================================================
// REPOSITORY - Handles S3 persistence
// ============================================================================

// S3MetadataRepository manages S3 metadata persistence
type S3MetadataRepository struct {
	objectStore ObjectStorer
	pathPrefix  string
}

// NewS3MetadataRepository creates a new repository
func NewS3MetadataRepository(objectStore ObjectStorer, vrgNamespace, vrgName string) *S3MetadataRepository {
	return &S3MetadataRepository{
		objectStore: objectStore,
		pathPrefix:  s3PathNamePrefix(vrgNamespace, vrgName),
	}
}

// Get retrieves metadata from S3
func (r *S3MetadataRepository) Get() (*S3StoreMetadata, bool, error) {
	metadataKey := r.pathPrefix + S3StoreMetadataKey

	var metadata S3StoreMetadata

	err := r.objectStore.DownloadObject(metadataKey, &metadata)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isAwsErrCodeNoSuchKey(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("failed to download metadata: %w", err)
	}

	return &metadata, true, nil
}

// Create creates new metadata in S3
func (r *S3MetadataRepository) Create(metadata *S3StoreMetadata) error {
	_, exists, err := r.Get()
	if err != nil {
		return fmt.Errorf("failed to check existing metadata: %w", err)
	}

	if exists {
		return fmt.Errorf("metadata already exists")
	}

	return r.save(metadata)
}

// Update updates existing metadata in S3 (creates if not exists)
func (r *S3MetadataRepository) Update(metadata *S3StoreMetadata) error {
	return r.save(metadata)
}

// Save saves metadata to S3 (internal)
func (r *S3MetadataRepository) save(metadata *S3StoreMetadata) error {
	metadataKey := r.pathPrefix + S3StoreMetadataKey

	if err := r.objectStore.UploadObject(metadataKey, metadata); err != nil {
		return fmt.Errorf("failed to upload metadata: %w", err)
	}

	return nil
}

// Delete removes metadata from S3
func (r *S3MetadataRepository) Delete() error {
	metadataKey := r.pathPrefix + S3StoreMetadataKey

	if err := r.objectStore.DeleteObject(metadataKey); err != nil {
		return fmt.Errorf("failed to delete metadata: %w", err)
	}

	return nil
}

// ============================================================================
// FACTORY - Creates initialized metadata
// ============================================================================

// NewS3StoreMetadata creates initialized metadata
func NewS3StoreMetadata(vrgName, vrgNamespace, s3ProfileName, clusterName string) *S3StoreMetadata {
	return &S3StoreMetadata{
		SchemaVersion:    S3StoreSchemaVersion,
		VRGName:          vrgName,
		VRGNamespace:     vrgNamespace,
		CurrentEpoch:     0,
		LastUpdatedBy:    clusterName,
		LastUpdatedAt:    metav1.Now(),
		ProtectionStatus: "InProgress",
		S3ProfileName:    s3ProfileName,
		ResourceTypes:    make(map[string]ResourceTypeInfo),
	}
}

func isAwsErrCodeNoSuchKey(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		return awsErr.Code() == s3.ErrCodeNoSuchKey
	}

	return false
}
