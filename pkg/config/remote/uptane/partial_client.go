// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package uptane

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"sync"

	"github.com/DataDog/datadog-agent/pkg/config/remote/meta"
	"github.com/DataDog/datadog-agent/pkg/proto/pbgo"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/theupdateframework/go-tuf/client"
	"github.com/theupdateframework/go-tuf/data"
	"github.com/theupdateframework/go-tuf/verify"
)

type partialClientRemoteStore struct {
	roots []*pbgo.TopMeta
}

func (s *partialClientRemoteStore) GetMeta(name string) (stream io.ReadCloser, size int64, err error) {
	metaPath, err := parseMetaPath(name)
	if err != nil {
		return nil, 0, err
	}
	if metaPath.role != roleRoot || !metaPath.versionSet {
		return nil, 0, client.ErrNotFound{File: name}
	}
	for _, root := range s.roots {
		if root.Version == metaPath.version {
			return ioutil.NopCloser(bytes.NewReader(root.Raw)), int64(len(root.Raw)), nil
		}
	}
	return nil, 0, client.ErrNotFound{File: name}
}

func (s *partialClientRemoteStore) GetTarget(path string) (stream io.ReadCloser, size int64, err error) {
	return nil, 0, client.ErrNotFound{File: path}
}

// PartialState represents the state of a partial uptane client
type PartialState struct {
	RootVersion    uint64
	TargetsVersion uint64
}

// PartialClient is a partial uptane client
type PartialClient struct {
	sync.Mutex

	rootClient  *client.Client
	localStore  client.LocalStore
	remoteStore *partialClientRemoteStore

	valid bool

	rootVersion    uint64
	targetsVersion uint64
	targetMetas    data.TargetFiles
	targetFiles    []*pbgo.File
}

// NewPartialClient creates a new partial uptane client
func NewPartialClient() (*PartialClient, error) {
	localStore := client.MemoryLocalStore()
	err := localStore.SetMeta("root.json", json.RawMessage(meta.RootsDirector().Last()))
	if err != nil {
		return nil, err
	}
	remoteStore := &partialClientRemoteStore{}
	c := &PartialClient{
		rootClient:  client.NewClient(localStore, remoteStore),
		localStore:  localStore,
		remoteStore: remoteStore,
		rootVersion: meta.RootsDirector().LastVersion(),
	}
	return c, nil
}

func (c *PartialClient) getRoot() (*data.Root, error) {
	metas, err := c.localStore.GetMeta()
	if err != nil {
		return nil, err
	}
	rawRoot := metas["root.json"]
	var signedRoot data.Signed
	err = json.Unmarshal(rawRoot, &signedRoot)
	if err != nil {
		return nil, err
	}
	var root data.Root
	err = json.Unmarshal(signedRoot.Signed, &root)
	if err != nil {
		return nil, err
	}
	return &root, nil
}

func (c *PartialClient) validateAndUpdateTargets(rawTargets []byte) error {
	if len(rawTargets) == 0 {
		return nil
	}
	root, err := c.getRoot()
	if err != nil {
		return err
	}
	db := verify.NewDB()
	for _, key := range root.Keys {
		for _, id := range key.IDs() {
			if err := db.AddKey(id, key); err != nil {
				return err
			}
		}
	}
	targetsRole, hasRoleTargets := root.Roles["targets"]
	if !hasRoleTargets {
		log.Warn("root is missing a targets role")
		return nil
	}
	role := &data.Role{Threshold: targetsRole.Threshold, KeyIDs: targetsRole.KeyIDs}
	if err := db.AddRole("targets", role); err != nil {
		log.Warnf("could not add targets role to db: %v", err)
		return nil
	}
	var targets data.Targets
	err = db.Unmarshal(rawTargets, &targets, "targets", 0)
	if err != nil {
		return err
	}
	c.targetMetas = targets.Targets
	c.targetsVersion = uint64(targets.Version)
	return nil
}

func (c *PartialClient) State() PartialState {
	c.Lock()
	defer c.Unlock()
	return PartialState{
		RootVersion:    c.rootVersion,
		TargetsVersion: c.targetsVersion,
	}
}

// Update updates the partial client
func (c *PartialClient) Update(response *pbgo.ClientGetConfigsResponse) error {
	c.Lock()
	defer c.Unlock()
	c.valid = false
	c.remoteStore.roots = response.Roots
	err := c.rootClient.UpdateRoots()
	if err != nil {
		return err
	}
	err = c.updateRootVersion()
	if err != nil {
		return err
	}
	err = c.validateAndUpdateTargets(response.Targets.Raw)
	if err != nil {
		return err
	}
	c.targetFiles = response.TargetFiles
	for _, target := range response.TargetFiles {
		_, err := c.targetFile(target.Path)
		if err != nil {
			return err
		}
	}
	c.valid = true
	return nil
}

func (c *PartialClient) updateRootVersion() error {
	meta, err := c.localStore.GetMeta()
	if err != nil {
		return err
	}
	rootMeta, rootFound := meta["root.json"]
	if !rootFound {
		return fmt.Errorf("could not find root.json in the local store")
	}
	version, err := metaVersion(rootMeta)
	if err != nil {
		return err
	}
	c.rootVersion = version
	return nil
}

func (c *PartialClient) Targets() (data.TargetFiles, error) {
	c.Lock()
	defer c.Unlock()
	if !c.valid {
		return nil, fmt.Errorf("partial client local repository is not in a valid state")
	}
	return c.targetMetas, nil
}

func (c *PartialClient) TargetFile(path string) ([]byte, error) {
	c.Lock()
	defer c.Unlock()
	if !c.valid {
		return nil, fmt.Errorf("partial client local repository is not in a valid state")
	}
	return c.targetFile(path)
}

func (c *PartialClient) targetFile(path string) ([]byte, error) {
	var targetFile *pbgo.File
	for _, target := range c.targetFiles {
		if target.Path == path {
			targetFile = target
		}
	}
	if targetFile == nil {
		return nil, fmt.Errorf("target file %s not found", path)
	}
	var targetMeta *data.TargetFileMeta
	for targetPath, target := range c.targetMetas {
		if targetPath == path {
			targetMeta = &target
		}
	}
	if targetMeta == nil {
		return nil, fmt.Errorf("target file meta %s not found", path)
	}
	if len(targetMeta.HashAlgorithms()) == 0 {
		return nil, fmt.Errorf("target file %s has no hash", path)
	}
	for _, algorithm := range targetMeta.HashAlgorithms() {
		var checksum []byte
		switch algorithm {
		case "sha256":
			sha256Checksum := sha256.Sum256(targetFile.Raw)
			checksum = sha256Checksum[:]
		case "sha512":
			sha512Checksum := sha512.Sum512(targetFile.Raw)
			checksum = sha512Checksum[:]
		default:
			return nil, fmt.Errorf("unsupported checksum %s", algorithm)
		}

		if !bytes.Equal(checksum, targetMeta.Hashes[algorithm]) {
			return nil, fmt.Errorf("target file %s has invalid checksum %x", path, checksum)
		}
	}
	return targetFile.Raw, nil
}
