package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"testing"
	"time"

	"github.com/DataDog/datadog-agent/pkg/api/security"
	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/config/remote/meta"
	"github.com/DataDog/datadog-agent/pkg/proto/pbgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/theupdateframework/go-tuf/data"
	"github.com/theupdateframework/go-tuf/sign"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type testServer struct {
	pbgo.UnimplementedAgentSecureServer
	mock.Mock
}

func (s *testServer) ClientGetConfigs(ctx context.Context, req *pbgo.ClientGetConfigsRequest) (*pbgo.ClientGetConfigsResponse, error) {
	args := s.Called(ctx, req)
	return args.Get(0).(*pbgo.ClientGetConfigsResponse), args.Error(1)
}

func getTestServer(t *testing.T) *testServer {
	hosts := []string{"127.0.0.1", "localhost", "::1"}
	_, rootCertPEM, rootKey, err := security.GenerateRootCert(hosts, 2048)
	require.NoError(t, err)
	rootKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rootKey),
	})
	cert, err := tls.X509KeyPair(rootCertPEM, rootKeyPEM)
	if err != nil {
		panic(err)
	}

	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	opts := []grpc.ServerOption{
		grpc.Creds(credentials.NewServerTLSFromCert(&cert)),
	}
	server := grpc.NewServer(opts...)
	testServer := &testServer{}
	pbgo.RegisterAgentSecureServer(server, testServer)

	go func() {
		if err := server.Serve(listener); err != nil {
			panic(err)
		}
	}()
	dir, err := os.MkdirTemp("", "testserver")
	require.NoError(t, err)
	config.Datadog.Set("auth_token_file_path", dir+"/auth_token")
	config.Datadog.Set("cmd_port", listener.Addr().(*net.TCPAddr).Port)
	_, err = security.CreateOrFetchToken()
	require.NoError(t, err)
	return testServer
}

func TestClientEmptyResponse(t *testing.T) {
	testServer := getTestServer(t)

	embeddedRoot := generateRoot(generateKey(), 1, generateKey())
	config.Datadog.Set("remote_configuration.director_root", embeddedRoot)

	testFacts := Facts{ID: "test-agent", Name: "test-agent-name", Version: "v6.1.1"}
	client, err := newClient(context.Background(), testFacts, []pbgo.Product{pbgo.Product_APM_SAMPLING})
	assert.NoError(t, err)

	testServer.On("ClientGetConfigs", mock.Anything, &pbgo.ClientGetConfigsRequest{Client: &pbgo.Client{
		State: &pbgo.ClientState{
			RootVersion:    meta.RootsDirector().LastVersion(),
			TargetsVersion: 0,
			Error:          "",
		},
		Id:       testFacts.ID,
		Name:     testFacts.Name,
		Version:  testFacts.Version,
		Products: []pbgo.Product{pbgo.Product_APM_SAMPLING},
	}}).Return(&pbgo.ClientGetConfigsResponse{
		Roots:       []*pbgo.TopMeta{},
		Targets:     &pbgo.TopMeta{},
		TargetFiles: []*pbgo.File{},
	}, nil)

	err = client.poll()
	assert.NoError(t, err)
}

func TestClientAPMUpdate(t *testing.T) {
	testServer := getTestServer(t)

	embeddedRoot := generateRoot(generateKey(), 1, generateKey())
	config.Datadog.Set("remote_configuration.director_root", embeddedRoot)

	testFacts := Facts{ID: "test-agent", Name: "test-agent-name", Version: "v6.1.1"}
	client, err := newClient(context.Background(), testFacts, []pbgo.Product{pbgo.Product_APM_SAMPLING})
	assert.NoError(t, err)

	testServer.On("ClientGetConfigs", mock.Anything, &pbgo.ClientGetConfigsRequest{Client: &pbgo.Client{
		State: &pbgo.ClientState{
			RootVersion:    meta.RootsDirector().LastVersion(),
			TargetsVersion: 0,
			Error:          "",
		},
		Id:       testFacts.ID,
		Name:     testFacts.Name,
		Version:  testFacts.Version,
		Products: []pbgo.Product{pbgo.Product_APM_SAMPLING},
	}}).Return(&pbgo.ClientGetConfigsResponse{
		Roots:       []*pbgo.TopMeta{},
		Targets:     &pbgo.TopMeta{},
		TargetFiles: []*pbgo.File{},
	}, nil)

	err = client.poll()
	assert.NoError(t, err)
}

func generateKey() *sign.PrivateKey {
	key, _ := sign.GenerateEd25519Key()
	return key
}

func generateTargets(key *sign.PrivateKey, version int, targets data.TargetFiles) []byte {
	meta := data.NewTargets()
	meta.Expires = time.Now().Add(1 * time.Hour)
	meta.Version = version
	meta.Targets = targets
	signed, _ := sign.Marshal(&meta, key.Signer())
	serialized, _ := json.Marshal(signed)
	return serialized
}

func generateRoot(key *sign.PrivateKey, version int, targetsKey *sign.PrivateKey) []byte {
	root := data.NewRoot()
	root.Version = version
	root.Expires = time.Now().Add(1 * time.Hour)
	root.AddKey(key.PublicData())
	root.AddKey(targetsKey.PublicData())
	root.Roles["root"] = &data.Role{
		KeyIDs:    key.PublicData().IDs(),
		Threshold: 1,
	}
	root.Roles["timestamp"] = &data.Role{
		KeyIDs:    key.PublicData().IDs(),
		Threshold: 1,
	}
	root.Roles["targets"] = &data.Role{
		KeyIDs:    targetsKey.PublicData().IDs(),
		Threshold: 1,
	}
	root.Roles["snapshot"] = &data.Role{
		KeyIDs:    key.PublicData().IDs(),
		Threshold: 1,
	}
	signedRoot, _ := sign.Marshal(&root, key.Signer())
	serializedRoot, _ := json.Marshal(signedRoot)
	return serializedRoot
}
