package main

import (
	"os"

	redis "github.com/fhitchen/vault-plugin-database-redis"
	hclog "github.com/hashicorp/go-hclog"
	dbplugin "github.com/hashicorp/vault/sdk/database/dbplugin/v5"
)

func main() {
	err := Run()
	if err != nil {
		logger := hclog.New(&hclog.LoggerOptions{})

		logger.Error("plugin shutting down", "error", err)
		os.Exit(1)
	}
}

// Run instantiates a RedisDB object, and runs the RPC server for the plugin
func Run() error {
	db, err := redis.New()
	if err != nil {
		return err
	}

	dbplugin.Serve(db.(dbplugin.Database))

	return nil
}
