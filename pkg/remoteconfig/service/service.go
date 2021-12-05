// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package service

import (
	"context"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/proto/pbgo"
	"github.com/DataDog/datadog-agent/pkg/remoteconfig/client"
	"github.com/DataDog/datadog-agent/pkg/remoteconfig/uptane"
	"github.com/DataDog/datadog-agent/pkg/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"go.etcd.io/bbolt"
)

const (
	minimalRefreshInterval = time.Second * 5
)

// Service defines the remote config management service responsible for fetching, storing
// and dispatching the configurations
type Service struct {
	sync.Mutex

	refreshInterval time.Duration
	remoteConfigKey remoteConfigKey

	ctx    context.Context
	db     *bbolt.DB
	uptane *uptane.Client
	client *client.HTTPClient

	products    map[pbgo.Product]struct{}
	newProducts map[pbgo.Product]struct{}

	subscribers []Subscriber
}

// NewService instantiates a new remote configuration management service
func NewService() (*Service, error) {
	refreshInterval := config.Datadog.GetDuration("remote_configuration.refresh_interval")
	if refreshInterval < minimalRefreshInterval {
		refreshInterval = minimalRefreshInterval
	}

	rawRemoteConfigKey := config.Datadog.GetString("remote_configuration.key")
	remoteConfigKey, err := parseRemoteConfigKey(rawRemoteConfigKey)
	if err != nil {
		return nil, err
	}

	apiKey := config.Datadog.GetString("api_key")
	if config.Datadog.IsSet("remote_configuration.api_key") {
		apiKey = config.Datadog.GetString("remote_configuration.api_key")
	}
	apiKey = config.SanitizeAPIKey(apiKey)
	hostname, err := util.GetHostname(context.Background())
	if err != nil {
		return nil, err
	}
	backendURL := config.Datadog.GetString("remote_configuration.endpoint")
	client := client.NewHTTPClient(backendURL, apiKey, remoteConfigKey.appKey, hostname)

	dbPath := path.Join(config.Datadog.GetString("run_path"), "remote-config.db")
	db, err := openCacheDB(dbPath)
	if err != nil {
		return nil, err
	}
	cacheKey := fmt.Sprintf("%s/%d/", remoteConfigKey.datacenter, remoteConfigKey.orgID)
	uptaneClient, err := uptane.NewClient(db, cacheKey, remoteConfigKey.orgID)
	if err != nil {
		return nil, err
	}

	return &Service{
		ctx:             context.Background(),
		refreshInterval: refreshInterval,
		remoteConfigKey: remoteConfigKey,
		products:        make(map[pbgo.Product]struct{}),
		newProducts:     make(map[pbgo.Product]struct{}),
		db:              db,
		client:          client,
		uptane:          uptaneClient,
	}, nil
}

// Start the remote configuration management service
func (s *Service) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()

		for {
			select {
			case <-time.After(s.refreshInterval):
				err := s.refresh()
				if err != nil {
					log.Errorf("could not refresh remote-config: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (s *Service) refresh() error {
	s.Lock()
	defer s.Unlock()
	previousState, err := s.uptane.State()
	if err != nil {
		return err
	}
	response, err := s.client.Fetch(s.ctx, previousState, s.products, s.newProducts)
	if err != nil {
		return err
	}
	err = s.uptane.Update(response)
	if err != nil {
		return err
	}
	for product := range s.newProducts {
		s.products[product] = struct{}{}
	}
	s.newProducts = make(map[pbgo.Product]struct{})
	currentState, err := s.uptane.State()
	if err != nil {
		return err
	}
	currentTargets, err := s.uptane.TargetsMeta()
	if err != nil {
		return err
	}
	subscriberUpdate := SubscriberUpdate{
		RootVersion: currentState.DirectorRootVersion,
		Targets:     currentTargets,
	}
	for _, subscriber := range s.subscribers {
		err := subscriber.Notify(subscriberUpdate)
		if err != nil {
			log.Errorf("could not notify a remote-config subscriber: %v", err)
		}
	}
	return nil
}

func (s *Service) Subscribe(subscriber Subscriber, products []pbgo.Product) {
	s.Lock()
	defer s.Unlock()
	s.subscribers = append(s.subscribers, subscriber)
	for _, product := range products {
		if _, ok := s.products[product]; ok {
			continue
		}
		s.newProducts[product] = struct{}{}
	}
}

func (s *Service) Unsubscribe(subscriber Subscriber) {
	s.Lock()
	defer s.Unlock()
	var subscribers []Subscriber
	for _, sub := range s.subscribers {
		if sub != subscriber {
			subscribers = append(subscribers, sub)
		}
	}
	s.subscribers = subscribers
}
