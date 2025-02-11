/*
Copyright 2023 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in
compliance with the License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is
distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions and limitations under the
License.
*/

package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/openshift-kni/oran-o2ims/internal/data"
	"github.com/openshift-kni/oran-o2ims/internal/k8s"
	"github.com/openshift-kni/oran-o2ims/internal/searchapi"
)

type ResourcePoolFetcher struct {
	logger        *slog.Logger
	cloudID       string
	backendURL    string
	backendToken  string
	backendClient *http.Client
	graphqlQuery  string
	graphqlVars   *searchapi.SearchInput
	resources     data.Stream
}

// ResourcePoolFetcherBuilder contains the data and logic needed to create a new ResourcePoolFetcher.
type ResourcePoolFetcherBuilder struct {
	logger           *slog.Logger
	transportWrapper func(http.RoundTripper) http.RoundTripper
	cloudID          string
	backendURL       string
	backendToken     string
	graphqlQuery     string
	graphqlVars      *searchapi.SearchInput
}

// NewResourcePoolFetcher creates a builder that can then be used to configure
// and create a handler for the ResourcePoolFetcher.
func NewResourcePoolFetcher() *ResourcePoolFetcherBuilder {
	return &ResourcePoolFetcherBuilder{}
}

// SetLogger sets the logger that the handler will use to write to the log. This is mandatory.
func (b *ResourcePoolFetcherBuilder) SetLogger(
	value *slog.Logger) *ResourcePoolFetcherBuilder {
	b.logger = value
	return b
}

// SetTransportWrapper sets the wrapper that will be used to configure the HTTP clients used to
// connect to other servers, including the backend server. This is optional.
func (b *ResourcePoolFetcherBuilder) SetTransportWrapper(
	value func(http.RoundTripper) http.RoundTripper) *ResourcePoolFetcherBuilder {
	b.transportWrapper = value
	return b
}

// SetCloudID sets the identifier of the O-Cloud of this handler. This is mandatory.
func (b *ResourcePoolFetcherBuilder) SetCloudID(
	value string) *ResourcePoolFetcherBuilder {
	b.cloudID = value
	return b
}

// SetBackendURL sets the URL of the backend server This is mandatory.
func (b *ResourcePoolFetcherBuilder) SetBackendToken(
	value string) *ResourcePoolFetcherBuilder {
	b.backendToken = value
	return b
}

// SetBackendToken sets the authentication token that will be used to authenticate to the backend
// server. This is mandatory.
func (b *ResourcePoolFetcherBuilder) SetBackendURL(
	value string) *ResourcePoolFetcherBuilder {
	b.backendURL = value
	return b
}

// SetGraphqlQuery sets the query to send to the search API server. This is mandatory.
func (b *ResourcePoolFetcherBuilder) SetGraphqlQuery(
	value string) *ResourcePoolFetcherBuilder {
	b.graphqlQuery = value
	return b
}

// SetGraphqlVars sets the query vars to send to the search API server. This is mandatory.
func (b *ResourcePoolFetcherBuilder) SetGraphqlVars(
	value *searchapi.SearchInput) *ResourcePoolFetcherBuilder {
	b.graphqlVars = value
	return b
}

// Build uses the data stored in the builder to create and configure a new handler.
func (b *ResourcePoolFetcherBuilder) Build() (
	result *ResourcePoolFetcher, err error) {
	// Check parameters:
	if b.logger == nil {
		err = errors.New("logger is mandatory")
		return
	}
	if b.cloudID == "" {
		err = errors.New("cloud identifier is mandatory")
		return
	}
	if b.backendURL == "" {
		err = errors.New("backend URL is mandatory")
		return
	}
	if b.backendToken == "" {
		err = errors.New("backend token is mandatory")
		return
	}

	// Create the HTTP client that we will use to connect to the backend:
	var backendTransport http.RoundTripper
	backendTransport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	if b.transportWrapper != nil {
		backendTransport = b.transportWrapper(backendTransport)
	}
	backendClient := &http.Client{
		Transport: backendTransport,
	}

	// Create and populate the object:
	result = &ResourcePoolFetcher{
		logger:        b.logger,
		cloudID:       b.cloudID,
		backendURL:    b.backendURL,
		backendToken:  b.backendToken,
		backendClient: backendClient,
		graphqlQuery:  b.graphqlQuery,
		graphqlVars:   b.graphqlVars,
	}
	return
}

// FetchItems returns a data stream of O2 ResourcePools.
// The items are converted from Clusters an Nodes fetched from the search API.
func (r *ResourcePoolFetcher) FetchItems(
	ctx context.Context) (resourcePools data.Stream, err error) {
	// Search Cluster and Nodes
	resultArr, err := r.getSearchResults(ctx)
	if err != nil {
		return
	}

	// Extract 'related' items array
	searchResult := resultArr[0].(map[string]any)
	relatedArr, err := data.GetArray(searchResult, "related")
	if err != nil {
		return
	}

	// Convert response to json
	items, err := json.Marshal(searchResult)
	if err != nil {
		return
	}
	itemsReader := bytes.NewReader(items)
	related, err := json.Marshal(relatedArr[0])
	if err != nil {
		return
	}
	relatedReader := bytes.NewReader(related)

	// Create reader for Clusters and Nodes
	clusters, err := k8s.NewStream().
		SetLogger(r.logger).
		SetReader(itemsReader).
		Build()
	if err != nil {
		return
	}
	nodes, err := k8s.NewStream().
		SetLogger(r.logger).
		SetReader(relatedReader).
		Build()

	// Transform Nodes to Resources
	r.resources = data.Map(nodes, r.mapNodeItem)

	// Transform Clusters to ResourcePools
	resourcePools = data.Map(clusters, r.mapClusterItem)

	return
}

func (r *ResourcePoolFetcher) getSearchResults(ctx context.Context) (result []any, err error) {
	// Convert GraphQL vars to a map
	var graphqlVars data.Object
	varsBytes, err := json.Marshal(r.graphqlVars)
	if err != nil {
		return
	}
	err = json.Unmarshal(varsBytes, &graphqlVars)
	if err != nil {
		return
	}

	// Build GraphQL request body
	var requestBody bytes.Buffer
	requestBodyObj := struct {
		Query     string      `json:"query"`
		Variables data.Object `json:"variables"`
	}{
		Query:     r.graphqlQuery,
		Variables: data.Object{"input": []data.Object{graphqlVars}},
	}
	err = json.NewEncoder(&requestBody).Encode(requestBodyObj)
	if err != nil {
		return
	}

	// Create http request
	request, err := http.NewRequest(http.MethodPost, r.backendURL, &requestBody)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.backendToken))
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := r.backendClient.Do(request)
	if err != nil {
		return
	}
	if response.StatusCode != http.StatusOK {
		r.logger.Error(
			"Received unexpected status code",
			"code", response.StatusCode,
			"url", r.backendURL,
		)
		err = fmt.Errorf(
			"received unexpected status code %d from '%s'",
			response.StatusCode, r.backendURL,
		)
		return
	}

	var responseMap data.Object
	if err := json.NewDecoder(response.Body).Decode(&responseMap); err != nil {
		return nil, err
	}

	// Extract 'data' obj
	responseData, err := data.GetObj(responseMap, "data")
	if err != nil {
		return
	}

	// Extract 'searchResult' array
	result, err = data.GetArray(responseData, "searchResult")
	if err != nil {
		return
	}
	return
}

// Map a Node to an O2 Resource object.
func (r *ResourcePoolFetcher) mapNodeItem(ctx context.Context,
	from data.Object) (to data.Object, err error) {
	resourceID, err := data.GetString(from, "_uid")
	if err != nil {
		return
	}

	description, err := data.GetString(from, "name")
	if err != nil {
		return
	}

	resourcePoolID, err := data.GetString(from, "cluster")
	if err != nil {
		return
	}

	labels, err := data.GetString(from, "label")
	if err != nil {
		// Skip object (could be a stale Node)
		return data.Object{}, nil
	}
	labelsMap := data.GetLabelsMap(labels)

	globalAssetID, err := data.GetString(from, "_systemUUID")
	if err != nil {
		return
	}

	to = data.Object{
		"resourceID":     resourceID,
		"resourceTypeID": "",
		"description":    description,
		"extensions":     labelsMap,
		"resourcePoolID": resourcePoolID,
		"globalAssetID":  globalAssetID,
	}
	return
}

// Map Cluster to an O2 ResourcePool object.
func (r *ResourcePoolFetcher) mapClusterItem(ctx context.Context,
	from data.Object) (to data.Object, err error) {
	resourcePoolID, err := data.GetString(from, "cluster")
	if err != nil {
		return
	}

	name, err := data.GetString(from, "name")
	if err != nil {
		return
	}

	labels, err := data.GetString(from, "label")
	if err != nil {
		return
	}
	labelsMap := data.GetLabelsMap(labels)

	resources, err := r.getResourceArray(ctx, r.resources, resourcePoolID)
	if err != nil {
		return
	}

	to = data.Object{
		"resourcePoolID": resourcePoolID,
		"name":           name,
		"oCloudID":       r.cloudID,
		"extensions":     labelsMap,
		"resources":      resources,
		// TODO: no direct mapping to a property in Cluster object
		"description":      "",
		"location":         "",
		"globalLocationID": "",
	}
	return
}

// Converts a specified data stream of O2 Resources (Nodes) to an array.
func (r *ResourcePoolFetcher) getResourceArray(ctx context.Context, items data.Stream,
	resourcePoolID string) ([]map[string]any, error) {
	selector := func(ctx context.Context, item data.Object) (result bool, err error) {
		// Filter by 'cluster' property
		// TODO: handle filtering when search API for global hub is available
		result = item["resourcePoolID"] == resourcePoolID
		return
	}
	return data.Collect(ctx, data.Select(items, selector))
}
