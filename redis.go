package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"errors"
	
	"github.com/mediocregopher/radix/v3"
	"github.com/mediocregopher/radix/v3/resp/resp2"
	"github.com/hashicorp/errwrap"
	hclog "github.com/hashicorp/go-hclog"
	dbplugin "github.com/hashicorp/vault/sdk/database/dbplugin/v5"
	"github.com/hashicorp/vault/sdk/database/helper/credsutil"
)

const (
	redisTypeName        = "redis"
	defaultRedisUserRule  = `["~*", "+@read"]`
	defaultTimeout       = 20000 * time.Millisecond
	maxKeyLength         = 64
)

var (
	_ dbplugin.Database = &RedisDB{}
)

// Type that combines the custom plugins Redis database connection configuration options and the Vault CredentialsProducer
// used for generating user information for the Redis database.
type RedisDB struct {
	*redisDBConnectionProducer
	credsutil.CredentialsProducer
}

// New implements builtinplugins.BuiltinFactory
func New() (interface{}, error) {
	db := new()
	// Wrap the plugin with middleware to sanitize errors
	dbType := dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues)
	return dbType, nil
}

func new() *RedisDB {
	connProducer := &redisDBConnectionProducer{}
	connProducer.Type = redisTypeName

	db := &RedisDB{
		redisDBConnectionProducer: connProducer,
	}

	return db
}

func (c *RedisDB) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	err := c.redisDBConnectionProducer.Initialize(ctx, req.Config, req.VerifyConnection)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	resp := dbplugin.InitializeResponse{
		Config: req.Config,
	}
	return resp, nil
}

func (c *RedisDB) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	// Grab the lock
	c.Lock()
	defer c.Unlock()

	username, err := credsutil.GenerateUsername(
		credsutil.DisplayName(req.UsernameConfig.DisplayName, maxKeyLength),
		credsutil.RoleName(req.UsernameConfig.RoleName, maxKeyLength))
	if err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to generate username: %w", err)
	}
	username = strings.ToUpper(username)

	db, err := c.getConnection(ctx)
	if err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to get connection: %w", err)
	}

	err = newUser(ctx, db, username, req)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	resp := dbplugin.NewUserResponse{
		Username: username,
	}

	return resp, nil
}

func (c *RedisDB) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Password != nil {
		err := c.changeUserPassword(ctx, req.Username, req.Password.NewPassword)
		return dbplugin.UpdateUserResponse{}, err
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (c *RedisDB) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	c.Lock()
	defer c.Unlock()

	db, err := c.getConnection(ctx) 
	if err != nil {
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("failed to make connection: %w", err)
	}

	// Close the database connection to ensure no new connections come in
	defer func() {
		if err := c.close(); err != nil {
			logger := hclog.New(&hclog.LoggerOptions{})
			logger.Error("defer close failed", "error", err)
		}
	}()

	var response string

	switch db.(type) {

	case *radix.Pool:
		err = db.Do(radix.Cmd(&response, "ACL", "DELUSER", req.Username))
		if err != nil {
			return dbplugin.DeleteUserResponse{}, fmt.Errorf("response from pool DeleteUser: %s, error: %w", response, err)
		}
	case *radix.Cluster:
		topo := db.(*radix.Cluster).Topo()
		nodes := topo.Map()
		for node := range nodes {
			cl, err := db.(*radix.Cluster).Client(node)
			err = cl.Do(radix.Cmd(&response, "ACL", "DELUSER", req.Username))
			if err != nil {
				return dbplugin.DeleteUserResponse{}, fmt.Errorf("response from cluster node %s for DeleteUser: %s, error: %w", node, response, err)
			}
			
		}
	}
	return dbplugin.DeleteUserResponse{}, nil
}

func newUser(ctx context.Context, db radix.Client, username string, req dbplugin.NewUserRequest) error {
	statements := removeEmpty(req.Statements.Commands)
	if len(statements) == 0 {
		statements = append(statements, defaultRedisUserRule)
	}
	// setup REDIS command
	aclargs := []string{"SETUSER", username, "ON", ">" + req.Password}

	var args []string
	err := json.Unmarshal([]byte(statements[0]), &args)
	if err != nil {
		return errwrap.Wrapf("error unmarshalling REDIS rules in the creation statement JSON: {{err}}", err)
	}

	// append the additional rules/permissions
	aclargs = append(aclargs, args...)

	var response string

	switch db.(type) {

	case *radix.Pool:
		err = db.Do(radix.Cmd(&response, "ACL", aclargs...))

		fmt.Printf("Response in newUser: %s\n", response)
	
		if err != nil {
			return err
		}
	case *radix.Cluster:
		topo := db.(*radix.Cluster).Topo()
		nodes := topo.Map()
		for node := range nodes {
			cl, err := db.(*radix.Cluster).Client(node)
			err = cl.Do(radix.Cmd(&response, "ACL", aclargs...))

			fmt.Printf("Response in cluster newUser: %s\n", response)
			
			if err != nil {
				return err
			}
			
		}
	}
	
	return nil
}

func (c *RedisDB) changeUserPassword(ctx context.Context, username, password string) error {
	c.Lock()
	defer c.Unlock()

	db, err := c.getConnection(ctx)
	if err != nil {
		return err
	}

	// Close the database connection to ensure no new connections come in
	defer func() {
		if err := c.close(); err != nil {
			logger := hclog.New(&hclog.LoggerOptions{})
			logger.Error("defer close failed", "error", err)
		}
	}()

	var response resp2.Array
	var redisErr resp2.Error
	mn := radix.MaybeNil{Rcv: &response}
	
	switch db.(type) {

	case *radix.Pool:

		err = db.Do(radix.Cmd(&mn, "ACL", "GETUSER", username))
		if errors.As(err, &redisErr) {
			fmt.Printf("redis error returned: %s", redisErr.E)
		}


		if err != nil {
			return fmt.Errorf("reset of passwords for user %s failed in changeUserPassword: %w", username, err)
		}

		if mn.Nil {
			return fmt.Errorf("changeUserPassword for user %s failed, user not found!", username);
		}

	case *radix.Cluster:
		topo := db.(*radix.Cluster).Topo()
		nodes := topo.Map()
		for node := range nodes {
			cl, err := db.(*radix.Cluster).Client(node)
			//err = cl.Do(radix.Cmd(&response, "ACL", "DELUSER", req.Username))
			//fmt.Printf("Response in cluster DeleteUser: %s\n", response)
			
			//if err != nil {
			//	return dbplugin.DeleteUserResponse{}, err
			//}
			err = cl.Do(radix.Cmd(&mn, "ACL", "GETUSER", username))
			if errors.As(err, &redisErr) {
				fmt.Printf("redis error returned: %s", redisErr.E)
			}
			
			
			if err != nil {
				return fmt.Errorf("reset of passwords for user %s failed in changeUserPassword on cluster member %s: %w", username, node, err)
			}
			
			if mn.Nil {
				return fmt.Errorf("changeUserPassword for user %s failed on cluster member %s, user not found!", node, username);
			}
		}
	}

	var sresponse string
	switch db.(type) {

	case *radix.Pool:
		err = db.Do(radix.Cmd(&sresponse, "ACL", "SETUSER", username, "RESETPASS", ">" + password))

		fmt.Printf("Response in changeUserPassword2: %s\n", sresponse)

		if err != nil {
			return fmt.Errorf("pool reset of password for user %s failed, REDIS response %s, error, %s", username, sresponse, err)
		}

	case *radix.Cluster:
		topo := db.(*radix.Cluster).Topo()
		nodes := topo.Map()
		for node := range nodes {
			cl, err := db.(*radix.Cluster).Client(node)

			err = cl.Do(radix.Cmd(&sresponse, "ACL", "SETUSER", username, "RESETPASS", ">" + password))

			fmt.Printf("Response in changeUserPassword2: %s\n", sresponse)

			if err != nil {
				return fmt.Errorf("cluster reset of password for user %s on node %s failed, REDIS response %s, error, %s", username, node, sresponse, err)
			}
		}
	}

	return nil
}

func removeEmpty(strs []string) []string {
	var newStrs []string
	for _, str := range strs {
		str = strings.TrimSpace(str)
		if str == "" {
			continue
		}
		newStrs = append(newStrs, str)
	}

	return newStrs
}

func computeTimeout(ctx context.Context) (timeout time.Duration) {
	deadline, ok := ctx.Deadline()
	if ok {
		return time.Until(deadline)
	}
	return defaultTimeout
}

func (c *RedisDB) getConnection(ctx context.Context) (radix.Client, error) {
	client, err := c.Connection(ctx)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (c *RedisDB) Type() (string, error) {
	return redisTypeName, nil
}
