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

package ocdav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typespb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	mockgateway "github.com/cs3org/go-cs3apis/mocks/github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	"github.com/cs3org/reva/v3/pkg/httpclient"
	"github.com/cs3org/reva/v3/pkg/rgrpc/todo/pool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/mock"
)

// Test that when calls come in to the PUT endpoint with a X-Disable-Versioning header,
// this header is propagated to the actual upload endpoint
func TestDisableVersioningHeaderPassedAlong(t *testing.T) {
	gatewayAPIEndpoint := "my-api-endpoint"
	incomingPath := "http://my-reva.com/myfile.txt"
	input := "Hello world!"
	request := httptest.NewRequest(http.MethodPut, incomingPath, strings.NewReader(input))
	request.Header.Add(HeaderContentLength, strconv.Itoa(len(input)))
	request.Header.Add(HeaderDisableVersioning, "true")

	calls := 0
	mockServerUpload := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if header := r.Header.Get(HeaderDisableVersioning); header == "" {
					t.Errorf("expected header %s but header was not set", HeaderDisableVersioning)
				}
				calls++
			},
		),
	)
	defer mockServerUpload.Close()
	endpointPath := mockServerUpload.URL

	gatewayClient := mockgateway.NewMockGatewayAPIClient(t)
	gatewayClient.On("Stat", mock.Anything, mock.Anything).Return(&provider.StatResponse{Status: &rpc.Status{Code: rpc.Code_CODE_NOT_FOUND}}, nil)
	gatewayClient.On("InitiateFileUpload", mock.Anything, mock.Anything).Return(&gateway.InitiateFileUploadResponse{
		Status: &rpc.Status{Code: rpc.Code_CODE_OK},
		Protocols: []*gateway.FileUploadProtocol{
			{Protocol: "simple", UploadEndpoint: endpointPath, Token: "my-secret-token"},
		}}, nil)
	pool.RegisterGatewayServiceClient(gatewayClient, gatewayAPIEndpoint)

	service := svc{
		c: &Config{
			GatewaySvc: gatewayAPIEndpoint,
		},
		client: httpclient.New(),
	}
	ref := provider.Reference{}
	w := httptest.NewRecorder()
	service.handlePut(context.Background(), w, request, &ref, zerolog.Logger{})
	if calls == 0 {
		t.Errorf("Upload endpoint was not called. ")
	}
}

func TestPutAllowsReferenceResources(t *testing.T) {
	gatewayAPIEndpoint := "reference-api-endpoint"
	calls := 0
	mockServerUpload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer mockServerUpload.Close()

	gatewayClient := mockgateway.NewMockGatewayAPIClient(t)
	gatewayClient.On("Stat", mock.Anything, mock.Anything).Return(&provider.StatResponse{
		Status: &rpc.Status{Code: rpc.Code_CODE_OK},
		Info: &provider.ResourceInfo{
			Type:  provider.ResourceType_RESOURCE_TYPE_REFERENCE,
			Id:    &provider.ResourceId{StorageId: "localfs", OpaqueId: "fileid-marie%2Fshared.txt"},
			Mtime: &typespb.Timestamp{},
		},
	}, nil)
	gatewayClient.On("InitiateFileUpload", mock.Anything, mock.Anything).Return(&gateway.InitiateFileUploadResponse{
		Status: &rpc.Status{Code: rpc.Code_CODE_OK},
		Protocols: []*gateway.FileUploadProtocol{
			{Protocol: "simple", UploadEndpoint: mockServerUpload.URL, Token: "my-secret-token"},
		},
	}, nil)
	pool.RegisterGatewayServiceClient(gatewayClient, gatewayAPIEndpoint)

	service := svc{
		c:      &Config{GatewaySvc: gatewayAPIEndpoint},
		client: httpclient.New(),
	}
	request := httptest.NewRequest(http.MethodPut, "http://my-reva.com/shared.txt", strings.NewReader("shared content"))
	request.Header.Add(HeaderContentLength, strconv.Itoa(len("shared content")))
	response := httptest.NewRecorder()

	service.handlePut(context.Background(), response, request, &provider.Reference{}, zerolog.Logger{})

	if response.Code != http.StatusNoContent {
		t.Fatalf("PUT response status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if calls != 1 {
		t.Fatalf("upload endpoint calls = %d, want 1", calls)
	}
}
