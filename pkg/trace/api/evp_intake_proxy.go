// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package api

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/info"
	"github.com/DataDog/datadog-agent/pkg/trace/log"
)

const (
	validSubdomainSymbols       = "_-."
	validPathSymbols            = "/_-+"
	validPathQueryStringSymbols = "/_-+@?&=.:\""
)

func isValidSubdomain(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune(validSubdomainSymbols, c) {
			return false
		}
	}
	return true
}

func isValidPath(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune(validPathSymbols, c) {
			return false
		}
	}
	return true
}

func isValidQueryString(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune(validPathQueryStringSymbols, c) {
			return false
		}
	}
	return true
}

// evpIntakeEndpointsFromConfig returns the configured list of endpoints to forward payloads to.
func evpIntakeEndpointsFromConfig(conf *config.AgentConfig) []config.Endpoint {
	mainEndpoint := config.Endpoint{
		Host:   conf.EvpIntakeProxy.DDURL,
		APIKey: conf.EvpIntakeProxy.APIKey,
	}
	endpoints := []config.Endpoint{mainEndpoint}
	for host, keys := range conf.EvpIntakeProxy.AdditionalEndpoints {
		for _, key := range keys {
			endpoints = append(endpoints, config.Endpoint{
				Host:   host,
				APIKey: key,
			})
		}
	}
	return endpoints
}

// evpIntakeHandler returns an HTTP handler for the /evpIntakeProxy API.
// Depending on the config, this is a proxying handler or a noop handler.
func (r *HTTPReceiver) evpIntakeHandler() http.Handler {

	// r.conf is populated by cmd/trace-agent/config/config.go
	if !r.conf.EvpIntakeProxy.Enabled {
		return evpIntakeErrorHandler("Has been disabled in config")
	}

	endpoints := evpIntakeEndpointsFromConfig(r.conf)
	reverseProxyHandler := evpIntakeReverseProxyHandler(r.conf, endpoints)
	return http.StripPrefix("/evpIntakeProxy/v1/", reverseProxyHandler)
}

// evpIntakeErrorHandler returns an HTTP handler that will always return
// http.StatusMethodNotAllowed along with a clarification.
func evpIntakeErrorHandler(message string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		msg := fmt.Sprintf("EvpIntakeProxy is disabled: %v", message)
		http.Error(w, msg, http.StatusMethodNotAllowed)
	})
}

// evpIntakeReverseProxyHandler creates an http.ReverseProxy which can forward payloads
// to one or more endpoints, based on the request received and the Agent configuration.
// Headers are not proxied, instead we add our own known set of headers.
// See also evpIntakeProxyTransport below.
func evpIntakeReverseProxyHandler(conf *config.AgentConfig, endpoints []config.Endpoint) http.Handler {
	director := func(req *http.Request) {

		containerID := req.Header.Get(headerContainerID)
		contentType := req.Header.Get("Content-Type")
		userAgent := req.Header.Get("User-Agent")

		// Clear all received headers
		req.Header = http.Header{}

		// Standard headers
		req.Header.Set("Via", fmt.Sprintf("trace-agent %s", info.Version))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("User-Agent", userAgent) // Set even if an empty string so Go doesn't set its default
		req.Header["X-Forwarded-For"] = nil     // Prevent setting X-Forwarded-For

		// Datadog headers
		if ctags := getContainerTags(conf.ContainerTags, containerID); ctags != "" {
			req.Header.Set("X-Datadog-Container-Tags", ctags)
		}
		req.Header.Set("X-Datadog-Hostname", conf.Hostname)
		req.Header.Set("X-Datadog-AgentDefaultEnv", conf.DefaultEnv)

		// URL, Host and the API key header are set in the transport for each outbound request
	}
	transport := conf.NewHTTPTransport()
	logger := log.NewThrottled(5, 10*time.Second) // limit to 5 messages every 10 seconds
	return &httputil.ReverseProxy{
		Director:  director,
		ErrorLog:  stdlog.New(logger, "EvpIntakeProxy: ", 0),
		Transport: &evpIntakeProxyTransport{transport, endpoints},
	}
}

// evpIntakeProxyTransport sends HTTPS requests to multiple targets using an
// underlying http.RoundTripper. API keys are set separately for each target.
// When multiple endpoints are in use the response from the first endpoint
// is proxied back to the client, while for all aditional endpoints the
// response is discarded.
type evpIntakeProxyTransport struct {
	transport http.RoundTripper
	endpoints []config.Endpoint
}

func (t *evpIntakeProxyTransport) RoundTrip(req *http.Request) (rresp *http.Response, rerr error) {

	// Parse request path: The first component is the target subdomain, the rest is the target path.
	inputPath := req.URL.Path
	subdomainAndPath := strings.SplitN(inputPath, "/", 2)
	if len(subdomainAndPath) != 2 || subdomainAndPath[0] == "" || subdomainAndPath[1] == "" {
		return nil, fmt.Errorf("EvpIntakeProxy: invalid path: '%s'", inputPath)
	}
	subdomain := subdomainAndPath[0]
	path := subdomainAndPath[1]

	// Sanitize the input
	if !isValidSubdomain(subdomain) {
		return nil, fmt.Errorf("EvpIntakeProxy: invalid subdomain: %s", subdomain)
	}
	if !isValidPath(path) {
		return nil, fmt.Errorf("EvpIntakeProxy: invalid target path: %s", path)
	}
	if !isValidQueryString(req.URL.RawQuery) {
		return nil, fmt.Errorf("EvpIntakeProxy: invalid query string: %s", req.URL.RawQuery)
	}

	req.URL.Scheme = "https"
	req.URL.Path = "/" + path

	setTarget := func(r *http.Request, host, apiKey string) {
		targetHost := subdomain + "." + host
		r.Host = targetHost
		r.URL.Host = targetHost
		r.Header.Set("DD-API-KEY", apiKey)
	}

	if len(t.endpoints) == 1 {
		setTarget(req, t.endpoints[0].Host, t.endpoints[0].APIKey)
		return t.transport.RoundTrip(req)
	}

	// There's more than one destination endpoint

	slurp, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	for i, endpointDomain := range t.endpoints {
		newreq := req.Clone(req.Context())
		newreq.Body = ioutil.NopCloser(bytes.NewReader(slurp))
		setTarget(newreq, endpointDomain.Host, endpointDomain.APIKey)
		if i == 0 {
			// given the way we construct the list of targets the main endpoint
			// will be the first one called, we return its response and error
			rresp, rerr = t.transport.RoundTrip(newreq)
			continue
		}

		if resp, err := t.transport.RoundTrip(newreq); err == nil {
			// we discard responses for all subsequent requests
			io.Copy(ioutil.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
		} else {
			log.Error(err)
		}
	}
	return rresp, rerr
}
