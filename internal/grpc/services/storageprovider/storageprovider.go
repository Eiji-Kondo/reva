// Copyright 2018-2024 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package storageprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	cachereg "github.com/cs3org/reva/v3/pkg/share/cache/registry"

	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/errtypes"
	"github.com/cs3org/reva/v3/pkg/mime"
	"github.com/cs3org/reva/v3/pkg/plugin"
	"github.com/cs3org/reva/v3/pkg/rgrpc"
	"github.com/cs3org/reva/v3/pkg/rgrpc/status"
	"github.com/cs3org/reva/v3/pkg/rgrpc/todo/pool"
	"github.com/cs3org/reva/v3/pkg/rhttp/router"
	"github.com/cs3org/reva/v3/pkg/share/cache"
	"github.com/cs3org/reva/v3/pkg/sharedconf"
	"github.com/cs3org/reva/v3/pkg/spaces"
	"github.com/cs3org/reva/v3/pkg/storage"
	"github.com/cs3org/reva/v3/pkg/storage/fs/registry"
	"github.com/cs3org/reva/v3/pkg/utils"
	"github.com/cs3org/reva/v3/pkg/utils/cfg"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

func init() {
	rgrpc.Register("storageprovider", New)
	plugin.RegisterNamespace("grpc.services.storageprovider.drivers", func(name string, newFunc any) {
		var f registry.NewFunc
		utils.Cast(newFunc, &f)
		registry.Register(name, f)
	})
}

type config struct {
	MountPath             string                    `docs:"/;The path where the file system would be mounted."                                                           mapstructure:"mount_path"`
	MountID               string                    `docs:"-;The ID of the mounted file system."                                                                         mapstructure:"mount_id"`
	Driver                string                    `docs:"localhome;The storage driver to be used."                                                                     mapstructure:"driver"`
	Drivers               map[string]map[string]any `docs:"url:pkg/storage/fs/localhome/localhome.go"                                                                    mapstructure:"drivers"`
	DataServerURL         string                    `docs:"http://localhost/data;The URL for the data server."                                                           mapstructure:"data_server_url"`
	ExposeDataServer      bool                      `docs:"false;Whether to expose data server."                                                                         mapstructure:"expose_data_server"` // if true the client will be able to upload/download directly to it
	AvailableXS           map[string]uint32         `docs:"nil;List of available checksums."                                                                             mapstructure:"available_checksums"`
	CustomMimeTypesJSON   string                    `docs:"nil;An optional mapping file with the list of supported custom file extensions and corresponding mime types." mapstructure:"custom_mime_types_json"`
	GatewaySvc            string                    `mapstructure:"gatewaysvc"`
	SpaceInfoCacheDriver  string                    `mapstructure:"space_info_cache_type"`
	SpaceInfoCacheTTL     int                       `mapstructure:"space_info_cache_ttl"`
	SpaceInfoCacheDrivers map[string]map[string]any `mapstructure:"space_info_caches"`
	ProvidesSpaceType     string                    `docs:"nil;Defines which type of spaces this storage provider provides (e.g. home, project, ...)."  mapstructure:"provides_space_type"`
	SpaceDepth            int                       `docs:"nil;Defines at which level spaces start. E.g. if spaces are located under '/eos/{space}', this would be 2. Any number lower than the depth of the SP's mount_path means only one space is provided by this StorageProvider."  mapstructure:"space_depth"`
}

func (c *config) ApplyDefaults() {
	if c.Driver == "" {
		c.Driver = "localhome"
	}

	if c.MountPath == "" {
		c.MountPath = "/"
	}

	if c.MountID == "" {
		c.MountID = "00000000-0000-0000-0000-000000000000"
	}

	if c.DataServerURL == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			c.DataServerURL = "http://0.0.0.0:19001/data"
		} else {
			c.DataServerURL = fmt.Sprintf("http://%s:19001/data", host)
		}
	}

	if c.SpaceInfoCacheDriver == "" {
		c.SpaceInfoCacheDriver = "memory_space"
	}

	// set sane defaults
	if len(c.AvailableXS) == 0 {
		c.AvailableXS = map[string]uint32{"md5": 100, "unset": 1000}
	}
	c.GatewaySvc = sharedconf.GetGatewaySVC(c.GatewaySvc)
}

type FSWithListRegexSuport interface {
	storage.FS
	ListWithRegex(ctx context.Context, path, regex string, depth uint, user *userpb.User) ([]*provider.ResourceInfo, error)
}

type service struct {
	conf               *config
	storage            storage.FS
	mountPath, mountID string
	dataServerURL      *url.URL
	availableXS        []*provider.ResourceChecksumPriority
}

func (s *service) Close() error {
	return s.storage.Shutdown(context.Background())
}

func (s *service) UnprotectedEndpoints() []string { return []string{} }

func (s *service) Register(ss *grpc.Server) {
	provider.RegisterProviderAPIServer(ss, s)
}

func parseXSTypes(xsTypes map[string]uint32) ([]*provider.ResourceChecksumPriority, error) {
	types := make([]*provider.ResourceChecksumPriority, 0, len(xsTypes))
	for xs, prio := range xsTypes {
		t := PKG2GRPCXS(xs)
		if t == provider.ResourceChecksumType_RESOURCE_CHECKSUM_TYPE_INVALID {
			return nil, errtypes.BadRequest("checksum type is invalid: " + xs)
		}
		xsPrio := &provider.ResourceChecksumPriority{
			Priority: prio,
			Type:     t,
		}
		types = append(types, xsPrio)
	}
	return types, nil
}

func registerMimeTypes(mappingFile string) error {
	if mappingFile != "" {
		f, err := os.ReadFile(mappingFile)
		if err != nil {
			return fmt.Errorf("storageprovider: error reading the custom mime types file: +%v", err)
		}
		mimeTypes := map[string]string{}
		err = json.Unmarshal(f, &mimeTypes)
		if err != nil {
			return fmt.Errorf("storageprovider: error unmarshalling the custom mime types file: +%v", err)
		}
		// register all mime types that were read
		for e, m := range mimeTypes {
			mime.RegisterMime(e, m)
		}
	}
	return nil
}

// New creates a new storage provider svc.
func New(ctx context.Context, m map[string]any) (rgrpc.Service, error) {
	log := appctx.GetLogger(ctx)
	var c config
	if err := cfg.Decode(m, &c); err != nil {
		return nil, err
	}

	mountPath := c.MountPath
	mountID := c.MountID

	fs, err := getFS(ctx, &c)
	if err != nil {
		return nil, err
	}

	if fs == nil {
		return nil, errors.New("error creating fs driver")
	}

	if c.SpaceDepth < pathLevels(mountPath) {
		log.Error().Msgf("the space_depth parameter should be set to at least the depth of the mount_path. For mount_path %s the space_depth should be set to at least %d", mountPath, pathLevels(mountPath))
	}

	// parse data server url
	u, err := url.Parse(c.DataServerURL)
	if err != nil {
		return nil, err
	}

	// validate available checksums
	xsTypes, err := parseXSTypes(c.AvailableXS)
	if err != nil {
		return nil, err
	}

	if len(xsTypes) == 0 {
		return nil, errtypes.NotFound("no available checksum, please set in config")
	}

	// read and register custom mime types if configured
	err = registerMimeTypes(c.CustomMimeTypesJSON)
	if err != nil {
		return nil, err
	}

	service := &service{
		conf:          &c,
		storage:       fs,
		mountPath:     mountPath,
		mountID:       mountID,
		dataServerURL: u,
		availableXS:   xsTypes,
	}

	return service, nil
}

func (s *service) SetArbitraryMetadata(ctx context.Context, req *provider.SetArbitraryMetadataRequest) (*provider.SetArbitraryMetadataResponse, error) {
	newRef, err := s.unwrap(ctx, req.Ref)
	if err != nil {
		err := errors.Wrap(err, "storageprovidersvc: error unwrapping path")
		return &provider.SetArbitraryMetadataResponse{
			Status: status.NewInternal(ctx, err, "error setting arbitrary metadata"),
		}, nil
	}

	if err := s.storage.SetArbitraryMetadata(ctx, newRef, req.ArbitraryMetadata); err != nil {
		var st *rpc.Status
		switch err.(type) {
		case errtypes.IsNotFound:
			st = status.NewNotFound(ctx, "path not found when setting arbitrary metadata")
		case errtypes.PermissionDenied:
			st = status.NewPermissionDenied(ctx, err, "permission denied")
		default:
			st = status.NewInternal(ctx, err, "error setting arbitrary metadata: "+req.Ref.String())
		}
		return &provider.SetArbitraryMetadataResponse{
			Status: st,
		}, nil
	}

	res := &provider.SetArbitraryMetadataResponse{
		Status: status.NewOK(ctx),
	}
	return res, nil
}

func (s *service) UnsetArbitraryMetadata(ctx context.Context, req *provider.UnsetArbitraryMetadataRequest) (*provider.UnsetArbitraryMetadataResponse, error) {
	log := appctx.GetLogger(ctx)
	newRef, err := s.unwrap(ctx, req.Ref)
	if err != nil {
		err := errors.Wrap(err, "storageprovidersvc: error unwrapping path")
		return &provider.UnsetArbitraryMetadataResponse{
			Status: status.NewInternal(ctx, err, "error unsetting arbitrary metadata"),
		}, nil
	}

	if err := s.storage.UnsetArbitraryMetadata(ctx, newRef, req.ArbitraryMetadataKeys); err != nil {
		var st *rpc.Status
		switch err.(type) {
		case errtypes.IsNotFound:
			st = status.NewNotFound(ctx, "path not found when unsetting arbitrary metadata")
		case errtypes.PermissionDenied:
			st = status.NewPermissionDenied(ctx, err, "permission denied")
		default:
			log.Error().Err(err).Str("ref", req.Ref.String()).Any("keys", req.ArbitraryMetadataKeys).Msg("error unsetting arbitrary metadata")
			st = status.NewInternal(ctx, err, "error unsetting arbitrary metadata: "+req.Ref.String())
		}
		return &provider.UnsetArbitraryMetadataResponse{
			Status: st,
		}, nil
	}

	res := &provider.UnsetArbitraryMetadataResponse{
		Status: status.NewOK(ctx),
	}
	return res, nil
}

// SetLock puts a lock on the given reference.
func (s *service) SetLock(ctx context.Context, req *provider.SetLockRequest) (*provider.SetLockResponse, error) {
	newRef, err := s.unwrap(ctx, req.Ref)
	if err != nil {
		err := errors.Wrap(err, "storageprovidersvc: error unwrapping path")
		return &provider.SetLockResponse{
			Status: status.NewInternal(ctx, err, "error setting lock"),
		}, nil
	}

	if err := s.storage.SetLock(ctx, newRef, req.Lock); err != nil {
		var st *rpc.Status
		switch err.(type) {
		case errtypes.IsNotFound:
			st = status.NewNotFound(ctx, "resource not found when setting lock")
		case errtypes.PermissionDenied:
			st = status.NewPermissionDenied(ctx, err, "permission denied")
		case errtypes.Conflict:
			st = status.NewFailedPrecondition(ctx, err, "reference already locked")
		default:
			st = status.NewInternal(ctx, err, "error setting lock: "+req.Ref.String())
		}
		return &provider.SetLockResponse{
			Status: st,
		}, nil
	}

	res := &provider.SetLockResponse{
		Status: status.NewOK(ctx),
	}
	return res, nil
}

// GetLock returns an existing lock on the given reference.
func (s *service) GetLock(ctx context.Context, req *provider.GetLockRequest) (*provider.GetLockResponse, error) {
	newRef, err := s.unwrap(ctx, req.Ref)
	if err != nil {
		err := errors.Wrap(err, "storageprovidersvc: error unwrapping path")
		return &provider.GetLockResponse{
			Status: status.NewInternal(ctx, err, "error getting lock"),
		}, nil
	}

	var lock *provider.Lock
	if lock, err = s.storage.GetLock(ctx, newRef); err != nil {
		var st *rpc.Status
		switch err.(type) {
		case errtypes.IsNotFound:
			st = status.NewNotFound(ctx, "reference or lock not found")
		case errtypes.PermissionDenied:
			st = status.NewPermissionDenied(ctx, err, "permission denied")
		default:
			st = status.NewInternal(ctx, err, "error getting lock: "+req.Ref.String())
		}
		return &provider.GetLockResponse{
			Status: st,
		}, nil
	}

	res := &provider.GetLockResponse{
		Status: status.NewOK(ctx),
		Lock:   lock,
	}
	return res, nil
}

// RefreshLock refreshes an existing lock on the given reference.
func (s *service) RefreshLock(ctx context.Context, req *provider.RefreshLockRequest) (*provider.RefreshLockResponse, error) {
	newRef, err := s.unwrap(ctx, req.Ref)
	if err != nil {
		err := errors.Wrap(err, "storageprovidersvc: error unwrapping path")
		return &provider.RefreshLockResponse{
			Status: status.NewInternal(ctx, err, "error refreshing lock"),
		}, nil
	}

	if err = s.storage.RefreshLock(ctx, newRef, req.Lock, req.ExistingLockId); err != nil {
		var st *rpc.Status
		switch err.(type) {
		case errtypes.IsNotFound:
			st = status.NewNotFound(ctx, "path not found when refreshing lock")
		case errtypes.PermissionDenied:
			st = status.NewPermissionDenied(ctx, err, "permission denied")
		case errtypes.BadRequest:
			st = status.NewFailedPrecondition(ctx, err, "reference not locked or caller does not hold the lock")
		default:
			st = status.NewInternal(ctx, err, "error refreshing lock: "+req.Ref.String())
		}
		return &provider.RefreshLockResponse{
			Status: st,
		}, nil
	}

	res := &provider.RefreshLockResponse{
		Status: status.NewOK(ctx),
	}
	return res, nil
}

// Unlock removes an existing lock from the given reference.
func (s *service) Unlock(ctx context.Context, req *provider.UnlockRequest) (*provider.UnlockResponse, error) {
	newRef, err := s.unwrap(ctx, req.Ref)
	if err != nil {
		err := errors.Wrap(err, "storageprovidersvc: error unwrapping path")
		return &provider.UnlockResponse{
			Status: status.NewInternal(ctx, err, "error on unlocking"),
		}, nil
	}

	if err = s.storage.Unlock(ctx, newRef, req.Lock); err != nil {
		var st *rpc.Status
		switch err.(type) {
		case errtypes.IsNotFound:
			st = status.NewNotFound(ctx, "path not found when unlocking")
		case errtypes.PermissionDenied:
			st = status.NewPermissionDenied(ctx, err, "permission denied")
		case errtypes.BadRequest:
			st = status.NewFailedPrecondition(ctx, err, "reference not locked")
		default:
			st = status.NewInternal(ctx, err, "error unlocking: "+req.Ref.String())
		}
		return &provider.UnlockResponse{
			Status: st,
		}, nil
	}

	res := &provider.UnlockResponse{
		Status: status.NewOK(ctx),
	}
	return res, nil
}

func (s *service) InitiateFileDownload(ctx context.Context, req *provider.InitiateFileDownloadRequest) (*provider.InitiateFileDownloadResponse, error) {
	// TODO(labkode): maybe add some checks before download starts? eg. check permissions?
	// TODO(labkode): maybe add short-lived token?
	// We now simply point the client to the data server.
	// For example, https://data-server.example.org/home/docs/myfile.txt
	// or ownclouds://data-server.example.org/home/docs/myfile.txt
	log := appctx.GetLogger(ctx)
	u := *s.dataServerURL
	log.Info().Str("data-server", u.String()).Interface("ref", req.Ref).Msg("file download")

	protocol := &provider.FileDownloadProtocol{Expose: s.conf.ExposeDataServer}

	if utils.IsRelativeReference(req.Ref) {
		protocol.Protocol = "spaces"
		u.Path = path.Join(u.Path, "spaces", req.Ref.ResourceId.StorageId+"!"+req.Ref.ResourceId.OpaqueId, req.Ref.Path)
	} else {
		newRef, err := s.unwrap(ctx, req.Ref)
		if err != nil {
			return &provider.InitiateFileDownloadResponse{
				Status: status.NewInternal(ctx, err, "error unwrapping path"),
			}, nil
		}
		// Currently, we only support the simple protocol for GET requests
		// Once we have multiple protocols, this would be moved to the fs layer
		protocol.Protocol = "simple"
		downloadPath := newRef.GetPath()
		if s.conf.ExposeDataServer {
			if info, err := s.storage.GetMD(ctx, newRef, nil); err == nil {
				mappedPath := exposedDownloadPath(downloadPath, info)
				if mappedPath != downloadPath {
					downloadPath = mappedPath
				}
			}
		}
			u.Path = path.Join(u.Path, "simple", downloadPath)  
				if req.Opaque != nil && req.Opaque.Map != nil {
				if vk := req.Opaque.Map["version_key"]; vk != nil {
					q := u.Query()
					q.Set("version_key", string(vk.Value)) 
					u.RawQuery = q.Encode()  
			}  
		}  
	}

	protocol.DownloadEndpoint = u.String()

	return &provider.InitiateFileDownloadResponse{
		Protocols: []*provider.FileDownloadProtocol{protocol},
		Status:    status.NewOK(ctx),
	}, nil
}
