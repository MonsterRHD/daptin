package server

import (
	"github.com/daptin/daptin/server/database"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/scim"
	"github.com/gin-gonic/gin"
)

const (
	scimEnableConfigKey = "scim.enable"
	scimTokenConfigKey  = "scim.admin_token"
)

// InitializeScimResources conditionally registers the SCIM 2.0 endpoint.
// The service exposes no routes until scim.enable is "true" and an
// scim.admin_token has been provisioned through the _config API.
func InitializeScimResources(router *gin.Engine, db database.DatabaseConnection,
	cruds map[string]*resource.DbResource, configStore *resource.ConfigStore) error {
	transaction, err := db.Beginx()
	if err != nil {
		return err
	}

	enable, err := configStore.GetConfigValueFor(scimEnableConfigKey, "backend", transaction)
	if err != nil {
		enable = "false"
		_ = configStore.SetConfigValueFor(scimEnableConfigKey, "false", "backend", transaction)
	}
	adminToken, tokenErr := configStore.GetConfigValueFor(scimTokenConfigKey, "backend", transaction)
	if tokenErr != nil {
		_ = configStore.SetConfigValueFor(scimTokenConfigKey, "", "backend", transaction)
	}
	if err := transaction.Commit(); err != nil {
		return err
	}

	if enable != "true" || adminToken == "" {
		return nil
	}

	store, err := scim.NewStore(cruds)
	if err != nil {
		return err
	}
	scim.RegisterRoutes(router.Group(scim.BasePath), store, adminToken)
	return nil
}
