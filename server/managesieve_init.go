package server

import (
	"fmt"
	"net"
	"os"

	"github.com/daptin/daptin/server/resource"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
)

// InitializeManageSieveResources resolves configuration, binds the listener
// and starts the RFC 5804 server. A nil server with no error means the
// service is disabled for this runtime. Initialization failures are returned so
// the caller can report them on the runtime error channel.
func InitializeManageSieveResources(configStore *resource.ConfigStore, transaction *sqlx.Tx,
	cruds map[string]*resource.DbResource, certificateManager *resource.CertificateManager,
	runtimeErrors chan<- error) (*resource.ManageSieveServer, error) {

	if os.Getenv("DAPTIN_DISABLE_MANAGESIEVE") == "true" {
		log.Infof("ManageSieve server is disabled since DAPTIN_DISABLE_MANAGESIEVE=true")
		return nil, nil
	}

	enabled, err := configStore.GetConfigValueFor("managesieve.enabled", "backend", transaction)
	if err != nil {
		enabled = "true"
		if storeErr := configStore.SetConfigValueFor("managesieve.enabled", enabled, "backend", transaction); storeErr != nil {
			return nil, storeErr
		}
	}
	if enabled != "true" {
		log.Infof("ManageSieve server is disabled in backend configuration")
		return nil, nil
	}

	address, err := configStore.GetConfigValueFor("managesieve.listen_interface", "backend", transaction)
	if err != nil {
		address = ":4190"
		if storeErr := configStore.SetConfigValueFor("managesieve.listen_interface", address, "backend", transaction); storeErr != nil {
			return nil, storeErr
		}
	}

	hostname := resolveIMAPHostname(configStore, transaction)
	cert, err := certificateManager.GetTLSConfig(hostname, true, transaction)
	if err != nil {
		return nil, fmt.Errorf("failed to get certificate for ManageSieve: %w", err)
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("failed to bind ManageSieve listener: %w", err)
	}

	server := &resource.ManageSieveServer{
		Listener:  listener,
		TLSConfig: cert.TLSConfig,
		Root:      cruds["mail"],
	}
	log.Printf("Starting ManageSieve server at %s: %v", address, hostname)
	go server.Serve(runtimeErrors)
	return server, nil
}
