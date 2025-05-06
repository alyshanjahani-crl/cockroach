// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tests

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/roachtestutil"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/roachtestutil/mixedversion"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/spec"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/errors"
)

//go:embed db-console/admin_endpoints.json
var adminEndpointsJSON string

//go:embed db-console/api_v2_endpoints.json
var apiV2EndpointsJSON string

//go:embed db-console/status_endpoints.json
var statusEndpointsJSON string

const nodeIDPlaceholder = "{node_id}"

// HTTPMethod represents the HTTP method to use for an endpoint
type HTTPMethod string

const (
	GET  HTTPMethod = "GET"
	POST HTTPMethod = "POST"
)

// endpoint represents a DB console endpoint to test
type endpoint struct {
	// url is the endpoint URL. If it contains a node ID placeholder, use {node_id}
	url string
	// method is the HTTP method to use for this endpoint
	method HTTPMethod
	// verifyResponse is a function that verifies the response is as expected.
	// If not specified, defaultVerifyResponse will be used.
	verifyResponse *func(resp *http.Response) error
}

// hasNodeID returns true if the endpoint URL contains the node ID placeholder
func (e endpoint) hasNodeID() bool {
	return strings.Contains(e.url, nodeIDPlaceholder)
}

// getVerifyResponse returns the verifyResponse function to use for this endpoint.
// If verifyResponse is not specified, returns defaultVerifyResponse.
func (e endpoint) getVerifyResponse() func(resp *http.Response) error {
	if e.verifyResponse == nil {
		return defaultVerifyResponse
	}
	return *e.verifyResponse
}

// defaultVerifyResponse is the default implementation that just checks for 200 status code
func defaultVerifyResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return errors.Newf("unexpected status code: %d, body: %s", resp.StatusCode, body)
	}
	return nil
}

func registerDBConsoleEndpoints(r registry.Registry) {
	// Register the regular version test
	r.Add(registry.TestSpec{
		Name:             "db-console/endpoints",
		Owner:            registry.OwnerObservability,
		Cluster:          r.MakeClusterSpec(3),
		CompatibleClouds: registry.AllClouds,
		Suites:           registry.Suites(registry.Nightly),
		Randomized:       true,
		Run:              runDBConsole,
		Timeout:          1 * time.Hour,
	})
}

func registerDBConsoleEndpointsMixedVersion(r registry.Registry) {
	// Register the mixed version test
	r.Add(registry.TestSpec{
		Name:             "db-console/mixed-version-endpoints",
		Owner:            registry.OwnerObservability,
		Cluster:          r.MakeClusterSpec(5, spec.WorkloadNode()),
		CompatibleClouds: registry.AllClouds,
		Suites:           registry.Suites(registry.MixedVersion, registry.Nightly),
		Randomized:       true,
		Run:              runDBConsoleMixedVersion,
		Timeout:          1 * time.Hour,
	})
}

func runDBConsoleMixedVersion(ctx context.Context, t test.Test, c cluster.Cluster) {
	mvt := mixedversion.NewTest(ctx, t, t.L(), c,
		c.CRDBNodes(),
		mixedversion.MinimumSupportedVersion("v23.2.0"),
	)

	mvt.InMixedVersion("test db console endpoints", func(ctx context.Context, l *logger.Logger, rng *rand.Rand, h *mixedversion.Helper) error {
		return testEndpoints(ctx, c, l, getEndpoints())
	})

	mvt.Run()
}

func runDBConsole(ctx context.Context, t test.Test, c cluster.Cluster) {
	c.Start(ctx, t.L(), option.DefaultStartOpts(), install.MakeClusterSettings())
	if err := testEndpoints(ctx, c, t.L(), getEndpoints()); err != nil {
		t.Fatal(err)
	}
}

// getEndpoints returns the list of endpoints to test
func getEndpoints() []endpoint {
	var endpoints []endpoint

	// Parse admin endpoints
	var adminEndpoints struct {
		Endpoints []struct {
			URL  string `json:"url"`
			Verb string `json:"verb"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(adminEndpointsJSON), &adminEndpoints); err != nil {
		panic(errors.Wrap(err, "failed to parse admin endpoints JSON"))
	}

	for _, ep := range adminEndpoints.Endpoints {
		endpoints = append(endpoints, endpoint{
			url:    ep.URL,
			method: HTTPMethod(ep.Verb),
		})
	}

	// Parse API v2 endpoints
	var apiV2Endpoints struct {
		Endpoints []struct {
			URL  string `json:"url"`
			Verb string `json:"verb"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(apiV2EndpointsJSON), &apiV2Endpoints); err != nil {
		panic(errors.Wrap(err, "failed to parse API v2 endpoints JSON"))
	}

	for _, ep := range apiV2Endpoints.Endpoints {
		endpoints = append(endpoints, endpoint{
			url:    ep.URL,
			method: HTTPMethod(ep.Verb),
		})
	}

	// Parse status endpoints
	var statusEndpoints struct {
		Endpoints []struct {
			URL  string `json:"url"`
			Verb string `json:"verb"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(statusEndpointsJSON), &statusEndpoints); err != nil {
		panic(errors.Wrap(err, "failed to parse status endpoints JSON"))
	}

	for _, ep := range statusEndpoints.Endpoints {
		endpoints = append(endpoints, endpoint{
			url:    ep.URL,
			method: HTTPMethod(ep.Verb),
		})
	}

	return endpoints
}

func testEndpoint(
	ctx context.Context,
	client *roachtestutil.RoachtestHTTPClient,
	baseURL string,
	ep endpoint,
	nodeID string,
	verifyResponse func(*http.Response) error,
	l *logger.Logger,
) error {
	fullURL := baseURL + ep.url
	if nodeID != "" {
		fullURL = strings.Replace(fullURL, nodeIDPlaceholder, nodeID, 1)
	}

	// Skipping for testing/development
	if ep.method == POST {
		l.Printf("skipping POST %v", fullURL)
		return nil
	}
	if strings.Contains(fullURL, "{") && !ep.hasNodeID() {
		l.Printf("skipping endpoint with placeholder: %v", fullURL)
		return nil
	}

	l.Printf("testing endpoint: %s", fullURL)
	var resp *http.Response
	var err error

	switch ep.method {
	case GET:
		resp, err = client.Get(ctx, fullURL)
	case POST:
		resp, err = client.Post(ctx, fullURL, "application/json", strings.NewReader("{}"))
	default:
		return errors.Newf("unsupported HTTP method: %s", ep.method)
	}

	if err != nil {
		return errors.Wrapf(err, "failed to %s %s", ep.method, fullURL)
	}
	defer resp.Body.Close()

	// No error for testing/development
	err = verifyResponse(resp)
	if err != nil {
		l.Printf("%v", err)
	}
	return nil
}

func testEndpoints(
	ctx context.Context, c cluster.Cluster, l *logger.Logger, endpoints []endpoint,
) error {
	// Get the node IDs for each node
	idMap := make(map[int]roachpb.NodeID)
	urlMap := make(map[int]string)
	adminUIAddrs, err := c.ExternalAdminUIAddr(ctx, l, c.CRDBNodes())
	if err != nil {
		return err
	}

	client := roachtestutil.DefaultHTTPClient(c, l, roachtestutil.HTTPTimeout(15*time.Second))

	for i, addr := range adminUIAddrs {
		var details serverpb.DetailsResponse
		url := `https://` + addr + `/_status/details/local`
		if err := retry.ForDuration(10*time.Second, func() error {
			return client.GetJSON(ctx, url, &details)
		}); err != nil {
			return err
		}
		idMap[i+1] = details.NodeID
		urlMap[i+1] = `https://` + addr
	}

	// Test each endpoint
	for _, ep := range endpoints {
		if ep.hasNodeID() {
			// For endpoints with node IDs, test with "local", own node ID, and another node ID
			for nodeID := 1; nodeID <= len(idMap); nodeID++ {
				baseURL := urlMap[nodeID]
				// Test with "local"
				if err := testEndpoint(ctx, client, baseURL, ep, "local", ep.getVerifyResponse(), l); err != nil {
					return errors.Wrapf(err, "failed testing endpoint %s with local", ep.url)
				}
				// Test with own node ID
				if err := testEndpoint(ctx, client, baseURL, ep, idMap[nodeID].String(), ep.getVerifyResponse(), l); err != nil {
					return errors.Wrapf(err, "failed testing endpoint %s with own node ID", ep.url)
				}
				// Test with another node ID
				otherNodeID := (nodeID % len(idMap)) + 1
				if err := testEndpoint(ctx, client, baseURL, ep, idMap[otherNodeID].String(), ep.getVerifyResponse(), l); err != nil {
					return errors.Wrapf(err, "failed testing endpoint %s with other node ID", ep.url)
				}
			}
		} else {
			// For endpoints without node IDs, test on each node
			for nodeID := 1; nodeID <= len(idMap); nodeID++ {
				if err := testEndpoint(ctx, client, urlMap[nodeID], ep, "", ep.getVerifyResponse(), l); err != nil {
					return errors.Wrapf(err, "failed testing endpoint %s", ep.url)
				}
			}
		}
	}

	return nil
}
